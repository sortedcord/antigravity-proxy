// Package oauth handles Google authorization and token exchange using configured
// or official consumer OAuth client credentials.
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

	"antigravity-proxy/internal/apikey"
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
	port, err := config.OAuthCallbackPort()
	if err != nil {
		return nil, 0, err
	}
	if port != 0 {
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
	return LoginWithOptions(cfg, path, SessionOptions{})
}

// LoginWithOptions authorizes Google credentials through a loopback callback using PKCE
// and state validation with custom session options, then saves them to path.
func LoginWithOptions(cfg config.Config, path string, opts SessionOptions) error {
	return loginWithOptions(cfg, path, os.Stdout, opts, nil)
}

func login(cfg config.Config, path string, output io.Writer) error {
	return loginWithClient(cfg, path, output, nil)
}

func loginWithClient(cfg config.Config, path string, output io.Writer, client *http.Client) error {
	return loginWithOptions(cfg, path, output, SessionOptions{}, client)
}

func loginWithOptions(cfg config.Config, path string, output io.Writer, opts SessionOptions, client *http.Client) error {
	manager := NewSessionManagerWithOptions(client, func(ctx context.Context, sessionOpts SessionOptions, tokens Tokens, clientID, clientSecret string, user UserInfo) (AccountConfigResult, error) {
		if err := ctx.Err(); err != nil {
			return AccountConfigResult{}, err
		}
		var mintedSecret, mintedKeyID string
		var newKey *config.AccountKey
		if sessionOpts.IssueKey != "" {
			var genErr error
			mintedSecret, genErr = apikey.Generate()
			if genErr != nil {
				return AccountConfigResult{}, genErr
			}
			mintedKeyID, genErr = apikey.GenerateID()
			if genErr != nil {
				return AccountConfigResult{}, genErr
			}
			newKey = &config.AccountKey{
				ID:        mintedKeyID,
				Label:     sessionOpts.IssueKey,
				Hash:      apikey.Hash(mintedSecret),
				Last4:     apikey.Last4(mintedSecret),
				CreatedAt: time.Now().UTC(),
			}
		}

		if sessionOpts.AccountID != "" || sessionOpts.IssueKey != "" || len(cfg.Accounts) > 0 {
			targetID := sessionOpts.AccountID
			accessToken := ""
			if tokens.RefreshToken == "" {
				accessToken = tokens.AccessToken
			}

			err := config.UpdateAccounts(path, func(accounts *[]config.Account) error {
				if targetID == "" {
					for _, da := range *accounts {
						if da.AccountID == user.ID {
							targetID = da.ID
							break
						}
					}
					if targetID == "" {
						existingIDs := make([]string, 0, len(*accounts))
						for _, da := range *accounts {
							existingIDs = append(existingIDs, da.ID)
						}
						targetID = config.DeriveAccountID(user.Email, existingIDs)
					}
				}

				for _, da := range *accounts {
					if da.AccountID == user.ID && da.ID != targetID {
						return ErrGoogleAccountAlreadyLinked
					}
					if da.ID == targetID && da.AccountID != "" && da.AccountID != user.ID && !sessionOpts.Replace {
						return ErrAccountIdentityMismatch
					}
				}

				found := false
				for i := range *accounts {
					if (*accounts)[i].ID == targetID {
						found = true
						(*accounts)[i].ApplyCredentials(config.Credentials{
							AccessToken:       accessToken,
							RefreshToken:      tokens.RefreshToken,
							OAuthClientID:     clientID,
							OAuthClientSecret: clientSecret,
							AccountID:         user.ID,
							AccountEmail:      user.Email,
							AccountName:       user.Name,
						})
						if newKey != nil {
							(*accounts)[i].Keys = append((*accounts)[i].Keys, *newKey)
						}
						return nil
					}
				}
				if !found {
					var keys []config.AccountKey
					if newKey != nil {
						keys = append(keys, *newKey)
					}
					newAcc := config.Account{
						ID:      targetID,
						Label:   targetID,
						Enabled: true,
						Keys:    keys,
					}
					newAcc.ApplyCredentials(config.Credentials{
						AccessToken:       accessToken,
						RefreshToken:      tokens.RefreshToken,
						OAuthClientID:     clientID,
						OAuthClientSecret: clientSecret,
						AccountID:         user.ID,
						AccountEmail:      user.Email,
						AccountName:       user.Name,
					})
					*accounts = append(*accounts, newAcc)
				}
				return nil
			})
			if err != nil {
				return AccountConfigResult{}, err
			}
			fmt.Fprintf(output, "Authorization saved for account %q (%s) to %s\n", targetID, user.Email, path)
			if mintedSecret != "" {
				fmt.Fprintf(output, "\nNew API key generated:\n  Key:   %s\n  ID:    %s\n  Label: %s\n\nSave this key now - it cannot be displayed again.\n", mintedSecret, mintedKeyID, sessionOpts.IssueKey)
			}
			return AccountConfigResult{AccountID: targetID, KeyID: mintedKeyID, KeySecret: mintedSecret}, nil
		}

		if err := saveLoginTokens(path, tokens, clientID, clientSecret, user); err != nil {
			return AccountConfigResult{}, err
		}
		if tokens.RefreshToken == "" {
			fmt.Fprintln(output, "Warning: Google did not return a refresh token; the saved access token expires.")
		}
		fmt.Fprintf(output, "Authorization saved to %s\n", path)
		return AccountConfigResult{AccountID: "default"}, nil
	})
	manager.cli = true
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), sessionLifetime)
	defer cancel()
	info, err := manager.BeginWithOptions(ctx, opts)
	if err != nil {
		return err
	}
	manager.mu.Lock()
	session := manager.active
	manager.mu.Unlock()
	if cfg.OAuthClientID != "" && cfg.OAuthClientID != session.clientID {
		fmt.Fprintln(output, "Authorizing with the consumer OAuth client; existing credentials remain unchanged until login succeeds.")
	}
	fmt.Fprintf(output, "Open this URL to authorize Antigravity Proxy:\n\n%s\n\nWaiting for the local callback...\n", info.AuthURL)
	select {
	case <-session.done:
		return session.result
	case <-ctx.Done():
		return errors.New("OAuth login timed out after 5 minutes")
	}
}

func saveLoginTokens(path string, tokens Tokens, clientID, clientSecret string, user UserInfo) error {
	credentials, err := LoginCredentials(tokens, clientID, clientSecret, user)
	if err != nil {
		return err
	}
	return config.UpdateCredentials(path, credentials)
}

// LoginCredentials constructs one validated replacement set for CLI persistence
// and runtime activation. Refreshable credentials never save an expiring static
// access token that would take precedence over their refresh token.
func LoginCredentials(tokens Tokens, clientID, clientSecret string, user UserInfo) (config.Credentials, error) {
	if strings.TrimSpace(tokens.AccessToken) == "" {
		return config.Credentials{}, errors.New("Google returned no access token")
	}
	if err := validateUserInfo(user); err != nil {
		return config.Credentials{}, err
	}
	credentials := config.Credentials{
		RefreshToken: tokens.RefreshToken, OAuthClientID: clientID, OAuthClientSecret: clientSecret,
		AccountID: user.ID, AccountEmail: user.Email, AccountName: user.Name,
	}
	if tokens.RefreshToken == "" {
		credentials.AccessToken = tokens.AccessToken
	}
	return credentials, nil
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
	return postTokenForm(ctx, client, form, "exchange OAuth code", "decode OAuth token response")
}

type authRequestParams struct {
	state    string
	verifier string
	authURL  string
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
		state:    state,
		verifier: verifier,
		authURL:  fmt.Sprintf("%s?%s", oauthAuthURL, query.Encode()),
	}, nil
}

// Refresh exchanges cfg.RefreshToken using only its saved or environment client
// pair. It never saves tokens.
func Refresh(ctx context.Context, cfg config.Config) (Tokens, error) {
	return RefreshWithClient(ctx, cfg, &http.Client{Timeout: DefaultHTTPTimeout})
}

// RefreshWithClient uses the supplied OAuth client without mutating global transports.
func RefreshWithClient(ctx context.Context, cfg config.Config, client *http.Client) (Tokens, error) {
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
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
	return postTokenForm(ctx, client, form, "refresh Google OAuth token", "decode Google OAuth refresh response")
}

func postTokenForm(ctx context.Context, client *http.Client, form url.Values, operation, decoding string) (Tokens, error) {
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return Tokens{}, fmt.Errorf("%s: token endpoint request failed", operation)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Tokens{}, fmt.Errorf("%s: Google returned HTTP %d (%s)", operation, resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	var tokens Tokens
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tokens); err != nil {
		return Tokens{}, fmt.Errorf("%s: invalid JSON response", decoding)
	}
	return tokens, nil
}
