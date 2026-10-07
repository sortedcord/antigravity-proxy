package oauth

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-proxy/internal/config"
)

func clearOAuthEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", "")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", "")
}

func syntheticPair(t *testing.T) (string, string) {
	t.Helper()
	return t.Name() + "-synthetic-id", t.Name() + "-synthetic-value"
}

func TestOAuthClientCredentialsUsesConfiguredPairWithoutDiscovery(t *testing.T) {
	clearOAuthEnvironment(t)
	id, secret := syntheticPair(t)
	cfg := config.Config{OAuthClientID: id, OAuthClientSecret: secret}
	gotID, gotSecret, err := oauthClientCredentials(cfg)
	if err != nil || gotID != id || gotSecret != secret {
		t.Fatal("configured pair was not resolved")
	}
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", "  "+id+"-override  ")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", "  "+secret+"-override  ")
	gotID, gotSecret, err = oauthClientCredentials(cfg)
	if err != nil || gotID != id+"-override" || gotSecret != secret+"-override" {
		t.Fatal("environment pair did not wholly replace configured pair")
	}
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", "")
	if _, _, err := oauthClientCredentials(cfg); err == nil {
		t.Fatal("partial environment pair was mixed with saved pair")
	}
}

func TestLoginPersistsPairOnlyWithSuccessfulTokens(t *testing.T) {
	clearOAuthEnvironment(t)
	id, secret := syntheticPair(t)
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Config{Port: 9191, ProjectID: t.Name(), AccessToken: "obsolete", RefreshToken: "obsolete"}
	if err := saveLoginTokens(cfg, path, Tokens{}, id, secret); err == nil {
		t.Fatal("login accepted missing access token")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("failed login wrote configuration")
	}
	for _, refresh := range []string{t.Name() + "-refresh", ""} {
		tokens := Tokens{AccessToken: t.Name() + "-access", RefreshToken: refresh}
		if err := saveLoginTokens(cfg, path, tokens, id, secret); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var saved config.Config
		if err := json.Unmarshal(data, &saved); err != nil {
			t.Fatal(err)
		}
		if saved.OAuthClientID != id || saved.OAuthClientSecret != secret || saved.RefreshToken != refresh || saved.Port != cfg.Port || saved.ProjectID != cfg.ProjectID {
			t.Fatal("login did not persist the exact pair with tokens and existing settings")
		}
		if (refresh == "" && saved.AccessToken != tokens.AccessToken) || (refresh != "" && saved.AccessToken != "") {
			t.Fatal("login selected the wrong saved token")
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("saved OAuth pair is not owner-only")
		}
	}
}

type oauthTestTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (transport oauthTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.URL.Scheme, clone.URL.Host = transport.target.Scheme, transport.target.Host
	return transport.base.RoundTrip(clone)
}

func TestRefreshUsesSavedPairAndWholeEnvironmentOverride(t *testing.T) {
	clearOAuthEnvironment(t)
	id, secret := syntheticPair(t)
	cfg := config.Config{OAuthClientID: id, OAuthClientSecret: secret, RefreshToken: t.Name() + "-refresh"}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantID, wantSecret := id, secret
		if calls.Add(1) == 2 {
			wantID, wantSecret = id+"-override", secret+"-override"
		}
		if err := r.ParseForm(); err != nil {
			t.Error("cannot decode refresh form")
		}
		if r.Method != http.MethodPost || r.Form.Get("client_id") != wantID || r.Form.Get("client_secret") != wantSecret || r.Form.Get("refresh_token") != cfg.RefreshToken || r.Form.Get("grant_type") != "refresh_token" {
			t.Error("refresh did not use the matching configured client pair")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Tokens{AccessToken: t.Name() + "-access", ExpiresIn: 3600})
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	http.DefaultTransport = oauthTestTransport{base: original, target: target}
	t.Cleanup(func() { http.DefaultTransport = original })
	for _, override := range []bool{false, true} {
		if override {
			t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", id+"-override")
			t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", secret+"-override")
		}
		tokens, err := Refresh(context.Background(), cfg)
		if err != nil || tokens.AccessToken == "" || tokens.ExpiresIn != 3600 {
			t.Fatal("refresh did not decode successful token response")
		}
	}
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", "")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", "")
	if _, err := Refresh(context.Background(), config.Config{RefreshToken: cfg.RefreshToken}); err == nil || !strings.Contains(err.Error(), "run antigravity-proxy login") {
		t.Fatal("refresh without saved pair did not require login")
	}
	if calls.Load() != 2 {
		t.Fatal("missing credentials contacted the token server")
	}
}

func TestExchangeOAuthCodeSendsMatchingPairAndPKCE(t *testing.T) {
	id, secret := syntheticPair(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error("cannot decode authorization form")
		}
		if r.Form.Get("client_id") != id || r.Form.Get("client_secret") != secret || r.Form.Get("code_verifier") != t.Name()+"-verifier" || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("redirect_uri") != "http://localhost:51121/oauth-callback" {
			t.Error("authorization exchange changed the client pair or PKCE parameters")
		}
		_, _ = io.WriteString(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh"}`)
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	http.DefaultTransport = oauthTestTransport{base: original, target: target}
	t.Cleanup(func() { http.DefaultTransport = original })
	tokens, err := exchangeOAuthCode(context.Background(), t.Name()+"-code", t.Name()+"-verifier", "http://localhost:51121/oauth-callback", id, secret)
	if err != nil || tokens.RefreshToken == "" {
		t.Fatal("authorization code exchange did not return refresh token")
	}
}

type loginTestOutput struct{ authorization chan string }

func (output loginTestOutput) Write(data []byte) (int, error) {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, oauthAuthURL+"?") {
			output.authorization <- line
		}
	}
	return len(data), nil
}

func TestLoginCallbackPersistsPairForLaterRefresh(t *testing.T) {
	for _, failAuthorization := range []bool{false, true} {
		t.Run(map[bool]string{false: "successful", true: "rejected"}[failAuthorization], func(t *testing.T) {
			clearOAuthEnvironment(t)
			id, secret := syntheticPair(t)
			cfg := config.Config{OAuthClientID: id, OAuthClientSecret: secret, ProjectID: t.Name()}
			if !failAuthorization {
				home := isolatedDiscoveryHome(t)
				data, nativeID, nativeSecret := syntheticNativeCLI(t)
				id, secret = nativeID, nativeSecret
				writeTestCLI(t, filepath.Join(home, "path", "agy"), data)
				cfg.OAuthClientID, cfg.OAuthClientSecret = "", ""
			}
			path := filepath.Join(t.TempDir(), "config.json")
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			listener.Close()
			t.Setenv("OAUTH_CALLBACK_PORT", strconv.Itoa(port))
			var calls atomic.Int32
			tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if err := r.ParseForm(); err != nil {
					t.Error("invalid token request form")
				}
				if r.Form.Get("client_id") != id || r.Form.Get("client_secret") != secret {
					t.Error("token exchange changed the selected OAuth pair")
				}
				grant := r.Form.Get("grant_type")
				if grant == "authorization_code" {
					if r.Form.Get("code_verifier") == "" || r.Form.Get("code") != t.Name()+"-code" {
						t.Error("login lost the callback code or PKCE verifier")
					}
				} else if grant != "refresh_token" || r.Form.Get("refresh_token") != t.Name()+"-refresh" {
					t.Error("later refresh did not use the saved token")
				}
				_ = json.NewEncoder(w).Encode(Tokens{AccessToken: t.Name() + "-access", RefreshToken: t.Name() + "-refresh", ExpiresIn: 3600})
			}))
			defer tokenServer.Close()
			target, err := url.Parse(tokenServer.URL)
			if err != nil {
				t.Fatal(err)
			}
			original := http.DefaultTransport
			http.DefaultTransport = oauthTestTransport{base: original, target: target}
			t.Cleanup(func() { http.DefaultTransport = original })
			output := loginTestOutput{authorization: make(chan string, 1)}
			result := make(chan error, 1)
			go func() { result <- login(cfg, path, output) }()
			var address string
			select {
			case address = <-output.authorization:
			case err := <-result:
				t.Fatalf("login stopped before callback: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("login did not publish authorization URL")
			}
			authorization, err := url.Parse(address)
			if err != nil {
				t.Fatal("invalid authorization URL")
			}
			query := authorization.Query()
			if query.Get("client_id") != id || query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" {
				t.Error("authorization URL lost selected client or PKCE")
			}
			callbackURL, err := url.Parse(query.Get("redirect_uri"))
			if err != nil {
				t.Fatal("invalid callback URL")
			}
			callbackQuery := url.Values{"state": {query.Get("state")}, "code": {t.Name() + "-code"}}
			if failAuthorization {
				callbackQuery.Set("error", "access_denied")
			}
			callbackURL.RawQuery = callbackQuery.Encode()
			callbackClient := &http.Client{Transport: original, Timeout: 5 * time.Second}
			response, err := callbackClient.Get(callbackURL.String())
			if err != nil {
				t.Fatal("could not deliver loopback callback")
			}
			response.Body.Close()
			select {
			case err = <-result:
			case <-time.After(5 * time.Second):
				t.Fatal("login did not finish callback exchange")
			}
			if failAuthorization {
				if err == nil || calls.Load() != 0 {
					t.Fatal("rejected authorization exchanged tokens")
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("rejected authorization persisted credentials")
				}
				return
			}
			if err != nil {
				t.Fatal("successful login failed")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var saved config.Config
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.OAuthClientID != id || saved.OAuthClientSecret != secret || saved.RefreshToken != t.Name()+"-refresh" || saved.ProjectID != cfg.ProjectID {
				t.Fatal("successful login did not save matching pair and refresh token")
			}
			tokens, err := Refresh(context.Background(), saved)
			if err != nil || tokens.AccessToken == "" || calls.Load() != 2 {
				t.Fatal("later refresh could not use the saved pair")
			}
		})
	}
}
