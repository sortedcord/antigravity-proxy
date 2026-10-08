// Package oauth handles Google authorization and token exchange using configured
// or safely extracted official CLI OAuth client credentials.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"antigravity-proxy/internal/config"
)

const (
	oauthAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	oauthTokenURL = "https://oauth2.googleapis.com/token"
)

func oauthClientCredentials(cfg config.Config) (string, string, error) {
	id, secret, err := cfg.OAuthCredentials()
	if err != nil {
		return "", "", err
	}
	if id == "" {
		return "", "", errors.New("no OAuth client credentials saved; run antigravity-proxy login or configure a complete environment pair")
	}
	return id, secret, nil
}

var oauthScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
}

// Tokens contains the credentials returned by Google's OAuth token endpoint.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	// ExpiresIn is the access-token lifetime in seconds reported by Google.
	ExpiresIn int64  `json:"expires_in"`
	TokenType string `json:"token_type"`
}

func listenOAuthCallbackPort() (net.Listener, int, error) {
	ports := []int{51121, 51122, 51123, 51124, 51125, 51126}
	if configured := os.Getenv("OAUTH_CALLBACK_PORT"); configured != "" {
		port, err := strconv.Atoi(configured)
		if err != nil || port < 1 || port > 65535 {
			return nil, 0, errors.New("OAUTH_CALLBACK_PORT must be an integer from 1 to 65535")
		}
		ports = append([]int{port}, ports...)
	}
	seen := make(map[int]bool, len(ports))
	var lastErr error
	for _, port := range ports {
		if seen[port] {
			continue
		}
		seen[port] = true
		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			return listener, port, nil
		}
		lastErr = err
	}
	return nil, 0, fmt.Errorf("could not bind OAuth callback port (tried %v): %w", ports, lastErr)
}

// Login authorizes Google credentials through a loopback callback using PKCE
// and state validation, then saves them to path while preserving other disk settings.
// An explicit environment pair overrides the official consumer credentials.
// Saved pairs are not reused for login. The selected pair is saved only after
// successful authorization.
func Login(cfg config.Config, path string) error {
	return login(cfg, path, os.Stdout)
}

func login(cfg config.Config, path string, output io.Writer) error {
	discoveryCtx, cancelDiscovery := context.WithTimeout(context.Background(), 30*time.Second)
	clientID, clientSecret, err := resolveLoginCredentials(discoveryCtx)
	cancelDiscovery()
	if err != nil {
		return err
	}
	if cfg.OAuthClientID != "" && cfg.OAuthClientID != clientID {
		fmt.Fprintln(output, "Authorizing with the consumer OAuth client; existing credentials remain unchanged until login succeeds.")
	}
	listener, callbackPort, err := listenOAuthCallbackPort()
	if err != nil {
		return err
	}
	defer listener.Close()
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/oauth-callback", callbackPort)

	authReq, err := buildAuthorizationRequest(clientID, redirectURI)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "Open this URL to authorize Antigravity Proxy:\n\n%s\n\nWaiting for the local callback on port %d...\n", authReq.authURL, callbackPort)

	type callback struct {
		code string
		err  error
	}
	result := make(chan callback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth-callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if received := r.URL.Query().Get("state"); received != authReq.state {
			http.Error(w, "OAuth state mismatch", http.StatusBadRequest)
			return
		}
		if oauthErr := r.URL.Query().Get("error"); oauthErr != "" {
			http.Error(w, "Authorization failed. You may close this tab.", http.StatusBadRequest)
			select {
			case result <- callback{err: errors.New("Google authorization failed")}:
			default:
			}
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing authorization code", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, "Authorization complete. You may close this tab and return to the terminal.\n")
		select {
		case result <- callback{code: code}:
		default:
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
		}
	}()

	var received callback
	select {
	case received = <-result:
	case err := <-serveErr:
		return fmt.Errorf("OAuth callback server stopped: %w", err)
	case <-time.After(5 * time.Minute):
		return errors.New("OAuth login timed out after 5 minutes")
	}
	if received.err != nil {
		return received.err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tokens, err := exchangeOAuthCode(ctx, received.code, authReq.verifier, redirectURI, clientID, clientSecret)
	if err != nil {
		return err
	}
	user, err := FetchUserInfo(ctx, tokens.AccessToken)
	if err != nil {
		return err
	}
	if err := saveLoginTokens(path, tokens, clientID, clientSecret, user); err != nil {
		return err
	}
	if tokens.RefreshToken == "" {
		fmt.Fprintln(os.Stderr, "Warning: Google did not return a refresh token; the saved access token expires.")
	}
	fmt.Fprintf(output, "Authorization saved to %s\n", path)
	return nil
}

func saveLoginTokens(path string, tokens Tokens, clientID, clientSecret string, user UserInfo) error {
	if strings.TrimSpace(tokens.AccessToken) == "" {
		return errors.New("Google returned no access token")
	}
	if err := validateUserInfo(user); err != nil {
		return err
	}
	accessToken := ""
	if tokens.RefreshToken == "" {
		accessToken = tokens.AccessToken
	}
	return config.UpdateCredentials(path, config.Credentials{
		AccessToken: accessToken, RefreshToken: tokens.RefreshToken,
		OAuthClientID: clientID, OAuthClientSecret: clientSecret,
		AccountID: user.ID, AccountEmail: user.Email, AccountName: user.Name,
	})
}

func exchangeOAuthCode(ctx context.Context, code, verifier, redirectURI, clientID, clientSecret string) (Tokens, error) {
	return exchangeOAuthCodeWithClient(ctx, &http.Client{Timeout: 30 * time.Second}, code, verifier, redirectURI, clientID, clientSecret)
}

func exchangeOAuthCodeWithClient(ctx context.Context, client *http.Client, code, verifier, redirectURI, clientID, clientSecret string) (Tokens, error) {
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"code_verifier": {verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return Tokens{}, errors.New("exchange OAuth code: token endpoint request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Tokens{}, fmt.Errorf("exchange OAuth code: Google returned HTTP %d (%s)", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	var tokens Tokens
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		return Tokens{}, errors.New("decode OAuth token response: invalid JSON response")
	}
	return tokens, nil
}

type authRequestParams struct {
	state     string
	verifier  string
	challenge string
	authURL   string
}

func buildAuthorizationRequest(clientID, redirectURI string) (authRequestParams, error) {
	stateBytes := make([]byte, 24)
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		return authRequestParams{}, err
	}
	if _, err := rand.Read(verifierBytes); err != nil {
		return authRequestParams{}, err
	}
	state := hex.EncodeToString(stateBytes)
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])

	query := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {strings.Join(oauthScopes, " ")},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	return authRequestParams{
		state:     state,
		verifier:  verifier,
		challenge: challenge,
		authURL:   fmt.Sprintf("%s?%s", oauthAuthURL, query.Encode()),
	}, nil
}

// Refresh exchanges cfg.RefreshToken using only its saved or environment client
// pair. It never saves tokens.
func Refresh(ctx context.Context, cfg config.Config) (Tokens, error) {
	return RefreshWithClient(ctx, cfg, &http.Client{Timeout: 30 * time.Second})
}

// RefreshWithClient uses the supplied OAuth client without mutating global transports.
func RefreshWithClient(ctx context.Context, cfg config.Config, client *http.Client) (Tokens, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	clientID, clientSecret, err := oauthClientCredentials(cfg)
	if err != nil {
		return Tokens{}, err
	}
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"refresh_token": {cfg.RefreshToken},
		"grant_type":    {"refresh_token"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return Tokens{}, errors.New("refresh Google OAuth token: token endpoint request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Tokens{}, fmt.Errorf("refresh Google OAuth token: Google returned HTTP %d (%s)", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	var tokens Tokens
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		return Tokens{}, errors.New("decode Google OAuth refresh response: invalid JSON response")
	}
	return tokens, nil
}
