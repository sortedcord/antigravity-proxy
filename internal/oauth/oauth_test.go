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

	"antigravity-proxy/internal/testutil"

	"antigravity-proxy/internal/config"
)

func clearOAuthEnvironment(t *testing.T) {
	t.Helper()
	testutil.UnsetEnv(t, "ANTIGRAVITY_ACCESS_TOKEN", "ANTIGRAVITY_REFRESH_TOKEN", "ANTIGRAVITY_OAUTH_CLIENT_ID", "ANTIGRAVITY_OAUTH_CLIENT_SECRET", "OAUTH_CALLBACK_PORT")
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
	if err := saveLoginTokens(path, Tokens{}, id, secret, testUserInfo()); err == nil {
		t.Fatal("login accepted missing access token")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("failed login wrote configuration")
	}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	for _, refresh := range []string{t.Name() + "-refresh", ""} {
		tokens := Tokens{AccessToken: t.Name() + "-access", RefreshToken: refresh}
		if err := saveLoginTokens(path, tokens, id, secret, testUserInfo()); err != nil {
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
		if saved.AccountID != testUserInfo().ID || saved.AccountEmail != testUserInfo().Email || saved.AccountName != testUserInfo().Name {
			t.Fatal("login did not persist the authenticated identity")
		}
		if (refresh == "" && saved.AccessToken != tokens.AccessToken) || (refresh != "" && saved.AccessToken != "") {
			t.Fatal("login selected the wrong saved token")
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatal("saved OAuth pair is not owner-only")
		}
	}
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
	client := testutil.RedirectClient(t, server.URL)
	for _, override := range []bool{false, true} {
		if override {
			t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", id+"-override")
			t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", secret+"-override")
		}
		tokens, err := RefreshWithClient(context.Background(), cfg, client)
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
		if r.Form.Get("client_id") != id || r.Form.Get("client_secret") != secret || r.Form.Get("code_verifier") != t.Name()+"-verifier" || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("redirect_uri") != "http://127.0.0.1:51121/oauth-callback" {
			t.Error("authorization exchange changed the client pair or PKCE parameters")
		}
		_, _ = io.WriteString(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh"}`)
	}))
	defer server.Close()
	client := testutil.RedirectClient(t, server.URL)
	tokens, err := exchangeOAuthCodeWithClient(context.Background(), client, t.Name()+"-code", t.Name()+"-verifier", "http://127.0.0.1:51121/oauth-callback", id, secret)
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
	for _, scenario := range []struct {
		name              string
		failAuthorization bool
		failToken         bool
	}{
		{name: "successful"},
		{name: "rejected", failAuthorization: true},
		{name: "token endpoint rejected", failToken: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			failAuthorization := scenario.failAuthorization
			clearOAuthEnvironment(t)
			oldID, oldSecret := syntheticPair(t)
			cfg := config.Config{OAuthClientID: oldID, OAuthClientSecret: oldSecret, RefreshToken: t.Name() + "-old-refresh", ProjectID: t.Name()}
			id, secret := t.Name()+"-client-id", t.Name()+"-client-secret"
			t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", id)
			t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", secret)
			path := filepath.Join(t.TempDir(), "config.json")
			if !failAuthorization {
				if err := config.Save(path, cfg); err != nil {
					t.Fatal(err)
				}
			}
			var before []byte
			if scenario.failToken {
				var err error
				before, err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			listener.Close()
			t.Setenv("OAUTH_CALLBACK_PORT", strconv.Itoa(port))
			var calls atomic.Int32
			tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth2/v2/userinfo" {
					if r.Header.Get("Authorization") != "Bearer "+t.Name()+"-access" {
						t.Error("profile request lost the exchanged access token")
					}
					_ = json.NewEncoder(w).Encode(testUserInfo())
					return
				}
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
				if scenario.failToken {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"error_description":"token-secret-must-not-escape"}`)
					return
				}
				_ = json.NewEncoder(w).Encode(Tokens{AccessToken: t.Name() + "-access", RefreshToken: t.Name() + "-refresh", ExpiresIn: 3600})
			}))
			defer tokenServer.Close()
			client := testutil.RedirectClient(t, tokenServer.URL)
			output := loginTestOutput{authorization: make(chan string, 1)}
			result := make(chan error, 1)
			go func() { result <- loginWithClient(cfg, path, output, client) }()
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
			if callbackURL.Hostname() != "127.0.0.1" {
				t.Fatal("callback redirect does not match the IPv4 loopback listener")
			}
			callbackClient := &http.Client{Timeout: 5 * time.Second}
			// Drain responses so connection replacement does not race the CLI's
			// graceful shutdown and exhaust the test's completion deadline.
			for _, stray := range []struct {
				method string
				query  url.Values
				status int
			}{
				{http.MethodGet, url.Values{"state": {"wrong-state"}, "code": {"stray-code"}}, http.StatusBadRequest},
				{http.MethodGet, url.Values{"state": {query.Get("state")}}, http.StatusBadRequest},
				{http.MethodPost, url.Values{"state": {query.Get("state")}, "code": {"stray-code"}}, http.StatusMethodNotAllowed},
			} {
				callbackURL.RawQuery = stray.query.Encode()
				request, err := http.NewRequest(stray.method, callbackURL.String(), nil)
				if err != nil {
					t.Fatal(err)
				}
				response, err := callbackClient.Do(request)
				if err != nil {
					t.Fatal("could not deliver stray callback")
				}
				if _, err := io.Copy(io.Discard, response.Body); err != nil {
					response.Body.Close()
					t.Fatal("could not read stray callback response", err)
				}
				response.Body.Close()
				if response.StatusCode != stray.status {
					t.Fatalf("stray callback status = %d, want %d", response.StatusCode, stray.status)
				}
				select {
				case err := <-result:
					t.Fatalf("stray callback finished login: %v", err)
				default:
				}
			}
			callbackQuery := url.Values{"state": {query.Get("state")}, "code": {t.Name() + "-code"}}
			if failAuthorization {
				callbackQuery.Set("error", "access_denied-secret-must-not-escape")
			}
			callbackURL.RawQuery = callbackQuery.Encode()
			response, err := callbackClient.Get(callbackURL.String())
			if err != nil {
				t.Fatal("could not deliver loopback callback")
			}
			if _, err := io.Copy(io.Discard, response.Body); err != nil {
				response.Body.Close()
				t.Fatal("could not read callback response", err)
			}
			response.Body.Close()
			wantStatus := http.StatusOK
			if failAuthorization || scenario.failToken {
				wantStatus = http.StatusBadRequest
			}
			if response.StatusCode != wantStatus {
				t.Fatalf("callback status = %d, want %d", response.StatusCode, wantStatus)
			}
			select {
			case err = <-result:
			case <-time.After(5 * time.Second):
				t.Fatal("login did not finish callback exchange")
			}
			if failAuthorization {
				if err != nil && strings.Contains(err.Error(), "secret-must-not-escape") {
					t.Fatal("authorization error reflected untrusted callback data")
				}
				if err == nil || calls.Load() != 0 {
					t.Fatal("rejected authorization exchanged tokens")
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("rejected authorization persisted credentials")
				}
				return
			}
			if scenario.failToken {
				if err == nil || strings.Contains(err.Error(), "secret-must-not-escape") || calls.Load() != 1 {
					t.Fatal("token endpoint failure leaked credentials or was not returned")
				}
				after, readErr := os.ReadFile(path)
				if readErr != nil || string(after) != string(before) {
					t.Fatal("failed token exchange changed saved configuration", readErr)
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
			tokens, err := RefreshWithClient(context.Background(), saved, client)
			if err != nil || tokens.AccessToken == "" || calls.Load() != 2 {
				t.Fatal("later refresh could not use the saved pair")
			}
		})
	}
}

func TestResolveLoginCredentials(t *testing.T) {
	cases := []struct {
		name       string
		envID      string
		envSecret  string
		wantID     string
		wantSecret string
		wantErr    bool
	}{
		{name: "default consumer", wantID: consumerClientID, wantSecret: consumerClientSecret},
		{name: "environment override", envID: "test-custom-id", envSecret: "test-custom-secret", wantID: "test-custom-id", wantSecret: "test-custom-secret"},
		{name: "partial environment", envID: "only-id", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearOAuthEnvironment(t)
			if tc.envID != "" {
				t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", tc.envID)
			}
			if tc.envSecret != "" {
				t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", tc.envSecret)
			}
			id, secret, err := resolveLoginCredentials()
			if (err != nil) != tc.wantErr {
				t.Fatalf("resolveLoginCredentials() error = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && (id != tc.wantID || secret != tc.wantSecret) {
				t.Fatalf("got id=%q secret=%q, want id=%q secret=%q", id, secret, tc.wantID, tc.wantSecret)
			}
		})
	}
}
