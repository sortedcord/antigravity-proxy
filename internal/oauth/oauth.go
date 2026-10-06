// Package oauth handles Google authorization and token exchange using
// runtime-provided OAuth client credentials.
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

func oauthClientCredentials() (string, string, error) {
	clientID := strings.TrimSpace(os.Getenv("ANTIGRAVITY_OAUTH_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET"))
	if clientID == "" || clientSecret == "" {
		return "", "", errors.New("ANTIGRAVITY_OAUTH_CLIENT_ID and ANTIGRAVITY_OAUTH_CLIENT_SECRET must be set")
	}
	return clientID, clientSecret, nil
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
// and state validation, then saves them to path while preserving other cfg settings.
// Client credentials come from ANTIGRAVITY_OAUTH_CLIENT_ID and
// ANTIGRAVITY_OAUTH_CLIENT_SECRET. It prefers saving a refresh token; if none
// is returned, it saves the expiring access token instead.
func Login(cfg config.Config, path string) error {
	clientID, clientSecret, err := oauthClientCredentials()
	if err != nil {
		return err
	}
	listener, callbackPort, err := listenOAuthCallbackPort()
	if err != nil {
		return err
	}
	defer listener.Close()
	redirectURI := fmt.Sprintf("http://localhost:%d/oauth-callback", callbackPort)

	stateBytes := make([]byte, 24)
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		return err
	}
	if _, err := rand.Read(verifierBytes); err != nil {
		return err
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
	fmt.Printf("Open this URL to authorize Antigravity Proxy:\n\n%s?%s\n\nWaiting for the local callback on port %d...\n", oauthAuthURL, query.Encode(), callbackPort)

	type callback struct {
		code string
		err  error
	}
	result := make(chan callback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth-callback", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if received := r.URL.Query().Get("state"); received != state {
			http.Error(w, "OAuth state mismatch", http.StatusBadRequest)
			select {
			case result <- callback{err: errors.New("OAuth state mismatch")}:
			default:
			}
			return
		}
		if oauthErr := r.URL.Query().Get("error"); oauthErr != "" {
			_, _ = io.WriteString(w, "Authorization failed. You may close this tab.\n")
			select {
			case result <- callback{err: fmt.Errorf("Google authorization failed: %s", oauthErr)}:
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
	defer server.Close()

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
	tokens, err := exchangeOAuthCode(ctx, received.code, verifier, redirectURI, clientID, clientSecret)
	if err != nil {
		return err
	}
	if tokens.AccessToken == "" {
		return errors.New("Google returned no access token")
	}
	cfg.AccessToken = ""
	cfg.RefreshToken = tokens.RefreshToken
	if cfg.RefreshToken == "" {
		cfg.AccessToken = tokens.AccessToken
		fmt.Fprintln(os.Stderr, "Warning: Google did not return a refresh token; the saved access token expires.")
	}
	if err := config.Save(path, cfg); err != nil {
		return err
	}
	fmt.Printf("Authorization saved to %s\n", path)
	return nil
}

func exchangeOAuthCode(ctx context.Context, code, verifier, redirectURI, clientID, clientSecret string) (Tokens, error) {
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
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return Tokens{}, fmt.Errorf("exchange OAuth code: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return Tokens{}, fmt.Errorf("exchange OAuth code: Google returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var tokens Tokens
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		return Tokens{}, fmt.Errorf("decode OAuth token response: %w", err)
	}
	return tokens, nil
}

// Refresh exchanges refreshToken using the runtime OAuth client credentials.
// The request is bound to ctx; returned tokens are not saved to configuration.
func Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	clientID, clientSecret, err := oauthClientCredentials()
	if err != nil {
		return Tokens{}, err
	}
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return Tokens{}, fmt.Errorf("refresh Google OAuth token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return Tokens{}, fmt.Errorf("refresh Google OAuth token: Google returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var tokens Tokens
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		return Tokens{}, fmt.Errorf("decode Google OAuth refresh response: %w", err)
	}
	return tokens, nil
}
