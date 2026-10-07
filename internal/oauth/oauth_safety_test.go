package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"antigravity-proxy/internal/config"
)

func oauthConfigHome(t *testing.T) string {
	t.Helper()
	for _, name := range []string{
		"HOST", "PORT", "API_KEY", "ANTIGRAVITY_ACCESS_TOKEN", "ANTIGRAVITY_REFRESH_TOKEN",
		"ANTIGRAVITY_OAUTH_CLIENT_ID", "ANTIGRAVITY_OAUTH_CLIENT_SECRET", "ANTIGRAVITY_PROJECT_ID",
		"ANTIGRAVITY_DAILY_ENDPOINT", "ANTIGRAVITY_PROD_ENDPOINT", "ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS",
		"ANTIGRAVITY_QUOTA_HISTORY_PATH", "ANTIGRAVITY_QUOTA_HISTORY_MAX_SAMPLES",
		"ANTIGRAVITY_CLIENT_VERSION", "ANTIGRAVITY_MAX_CONCURRENT_GENERATIONS",
	} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	return filepath.Join(home, ".config", "antigravity-proxy", "config.json")
}

func TestLoginPersistenceDoesNotLeakEnvironmentOrDefaults(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", exists), func(t *testing.T) {
			path := oauthConfigHome(t)
			original := []byte(`{"host":"127.0.0.1","apiKey":"saved-key","projectId":"original","quotaHistoryPath":"saved-history.jsonl","quotaHistoryMaxSamples":41,"maxConcurrentGenerations":3,"clientVersion":"saved-version","accessToken":"old-access","refreshToken":"old-refresh"}`)
			if exists {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			for name, value := range map[string]string{
				"HOST": "0.0.0.0", "API_KEY": "environment-only-secret", "PORT": "9191",
				"ANTIGRAVITY_PROJECT_ID": "env-project", "ANTIGRAVITY_QUOTA_HISTORY_PATH": "/home/app/environment-history.jsonl",
				"ANTIGRAVITY_QUOTA_HISTORY_MAX_SAMPLES": "91", "ANTIGRAVITY_MAX_CONCURRENT_GENERATIONS": "9",
				"ANTIGRAVITY_CLIENT_VERSION": "environment-version", "ANTIGRAVITY_ACCESS_TOKEN": "environment-token",
			} {
				t.Setenv(name, value)
			}
			cfg, loadedPath, err := config.Load()
			if err != nil || loadedPath != path || cfg.APIKey != "environment-only-secret" || cfg.QuotaHistoryPath != "/home/app/environment-history.jsonl" {
				t.Fatal("test did not load environment-overridden login configuration", err)
			}
			// A setting edited while the browser is open must not be reverted to the
			// configuration snapshot loaded before authorization.
			if exists {
				original = bytes.ReplaceAll(original, []byte(`"original"`), []byte(`"edited-during-login"`))
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := saveLoginTokens(path, Tokens{AccessToken: "new-access", RefreshToken: "new-refresh"}, "new-id", "new-secret"); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var saved map[string]json.RawMessage
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if string(saved["refreshToken"]) != `"new-refresh"` || string(saved["oauthClientId"]) != `"new-id"` || string(saved["oauthClientSecret"]) != `"new-secret"` {
				t.Fatal("login did not save its matching credentials")
			}
			for _, name := range []string{"accessToken", "refreshToken", "oauthClientId", "oauthClientSecret"} {
				delete(saved, name)
			}
			want := make(map[string]json.RawMessage)
			if exists {
				if err := json.Unmarshal(original, &want); err != nil {
					t.Fatal(err)
				}
				delete(want, "accessToken")
				delete(want, "refreshToken")
			}
			if !reflect.DeepEqual(saved, want) {
				t.Fatal("login changed unrelated disk fields or persisted defaults/environment")
			}
		})
	}
}

func TestLoginPersistenceRejectsUnsafeDiskWithoutChangingIt(t *testing.T) {
	for _, contents := range []string{`{"typoSetting":"keep"}`, `{} {}`, `null`, `{"host":`} {
		t.Run(contents, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if err := saveLoginTokens(path, Tokens{AccessToken: "new"}, "id", "secret"); err == nil {
				t.Fatal("login overwrote invalid disk configuration")
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != contents {
				t.Fatal("failed login changed disk settings", err)
			}
		})
	}
	if runtime.GOOS == "windows" {
		return
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := saveLoginTokens(path, Tokens{AccessToken: "new"}, "id", "secret"); err == nil {
		t.Fatal("login accepted a nonprivate saved configuration")
	}
}

func TestConcurrentLoginPersistenceKeepsMatchingCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"projectId":"keep","quotaHistoryMaxSamples":33}`), 0600); err != nil {
		t.Fatal(err)
	}
	const writers = 24
	results := make(chan error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			value := fmt.Sprintf("login-%d", i)
			results <- saveLoginTokens(path, Tokens{AccessToken: value, RefreshToken: value}, value, value)
		}(i)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved config.Config
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.OAuthClientID != saved.OAuthClientSecret || saved.OAuthClientID != saved.RefreshToken || saved.AccessToken != "" || saved.ProjectID != "keep" || saved.QuotaHistoryMaxSamples != 33 {
		t.Fatal("concurrent logins lost settings or mismatched token and client pair")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatal("concurrent login left temporary files", err)
	}
}

type failingOAuthTransport struct{}

func (failingOAuthTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("transport-secret-must-not-escape")
}

func TestOAuthFailuresDoNotExposeTokenResponseOrTransportDetails(t *testing.T) {
	clearOAuthEnvironment(t)
	id, secret := syntheticPair(t)
	cfg := config.Config{OAuthClientID: id, OAuthClientSecret: secret, RefreshToken: "saved-refresh"}
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"text failure", 400, "access_token=body-secret-must-not-escape"},
		{"JSON failure", 401, `{"error_description":"body-secret-must-not-escape","refresh_token":"private"}`},
		{"invalid token JSON", 200, `{"access_token":{"body-secret-must-not-escape":true}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			target, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			http.DefaultTransport = oauthTestTransport{base: original, target: target}
			for _, exchange := range []func() (Tokens, error){
				func() (Tokens, error) {
					return exchangeOAuthCode(context.Background(), "code", "verifier", "http://127.0.0.1/callback", id, secret)
				},
				func() (Tokens, error) { return Refresh(context.Background(), cfg) },
			} {
				tokens, err := exchange()
				if err == nil || tokens.AccessToken != "" || strings.Contains(err.Error(), "must-not-escape") {
					t.Fatal("OAuth failure exposed raw response data or accepted failed tokens")
				}
			}
		})
	}
	http.DefaultTransport = failingOAuthTransport{}
	if _, err := Refresh(context.Background(), cfg); err == nil || strings.Contains(err.Error(), "must-not-escape") {
		t.Fatal("OAuth refresh exposed transport error details")
	}
	if _, err := exchangeOAuthCode(context.Background(), "code", "verifier", "http://127.0.0.1/callback", id, secret); err == nil || strings.Contains(err.Error(), "must-not-escape") {
		t.Fatal("OAuth code exchange exposed transport error details")
	}
}

func TestLoginPersistenceCanonicalizesUniqueCredentialAliases(t *testing.T) {
	for _, refreshToken := range []string{"new-refresh", ""} {
		for index, contents := range []string{
			`{"AccessToken":"old-access","REFRESHTOKEN":"old-refresh","OAuthClientID":"old-id","OAUTHCLIENTSECRET":"old-secret","ProjectID":"keep"}`,
			`{"access\u0054oken":"old-access","RefreshToken":"old-refresh","oauthClient\u0049d":"old-id","OAuthClientSecret":"old-secret","ProjectID":"keep"}`,
		} {
			t.Run(fmt.Sprintf("refresh=%t/case-%d", refreshToken != "", index), func(t *testing.T) {
				path := oauthConfigHome(t)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
				if err := saveLoginTokens(path, Tokens{AccessToken: "new-access", RefreshToken: refreshToken}, "new-id", "new-secret"); err != nil {
					t.Fatal(err)
				}
				cfg, _, err := config.Load()
				if err != nil {
					t.Fatal(err)
				}
				wantAccess := ""
				if refreshToken == "" {
					wantAccess = "new-access"
				}
				if cfg.AccessToken != wantAccess || cfg.RefreshToken != refreshToken || cfg.OAuthClientID != "new-id" || cfg.OAuthClientSecret != "new-secret" || cfg.ProjectID != "keep" {
					t.Fatal("login retained obsolete case-variant credentials or changed unrelated settings")
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(data, &fields); err != nil {
					t.Fatal(err)
				}
				if string(fields["ProjectID"]) != `"keep"` {
					t.Fatal("credential replacement normalized an unrelated saved key")
				}
				for key := range fields {
					for _, canonical := range []string{"accessToken", "refreshToken", "oauthClientId", "oauthClientSecret"} {
						if strings.EqualFold(key, canonical) && key != canonical {
							t.Fatalf("login retained obsolete credential alias %q", key)
						}
					}
				}
			})
		}
	}
}
