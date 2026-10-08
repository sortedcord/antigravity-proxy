package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/oauth"
	"antigravity-proxy/internal/quota"
	"antigravity-proxy/internal/status"
)

type runtimeTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (tr runtimeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme, clone.URL.Host = tr.target.Scheme, tr.target.Host
	return tr.base.RoundTrip(clone)
}

type runtimeProvider struct {
	server          *httptest.Server
	client          *http.Client
	profileGate     func(*http.Request)
	projectGate     func(*http.Request)
	refreshCalls    atomic.Int32
	profileFailNext atomic.Bool
	requests        atomic.Int32
}

func newRuntimeProvider(t *testing.T) *runtimeProvider {
	t.Helper()
	provider := &runtimeProvider{}
	provider.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		provider.requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		account := "a"
		if strings.HasPrefix(token, "token-b") {
			account = "b"
		}
		switch req.URL.Path {
		case "/token":
			if err := req.ParseForm(); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			if req.Form.Get("grant_type") == "refresh_token" {
				provider.refreshCalls.Add(1)
				if req.Form.Get("refresh_token") != "refresh-b" {
					t.Error("wrong account refresh token")
					w.WriteHeader(400)
					return
				}
				_ = json.NewEncoder(w).Encode(oauth.Tokens{AccessToken: "token-b-renewed", ExpiresIn: 120})
			} else {
				_ = json.NewEncoder(w).Encode(oauth.Tokens{AccessToken: "token-b", RefreshToken: "refresh-b", ExpiresIn: 120})
			}
		case "/oauth2/v2/userinfo":
			if provider.profileFailNext.Swap(false) {
				http.Error(w, "temporary profile failure", http.StatusServiceUnavailable)
				return
			}
			if provider.profileGate != nil {
				provider.profileGate(req)
			}
			_ = json.NewEncoder(w).Encode(oauth.UserInfo{ID: account, Email: account + "@example.invalid", Name: "Account " + account})
		case "/v1internal:loadCodeAssist":
			if provider.projectGate != nil {
				provider.projectGate(req)
			}
			data := map[string]any{"cloudaicompanionProject": "project-" + account, "currentTier": map[string]string{"id": "free-tier", "name": "Antigravity"}}
			if account == "b" {
				data["paidTier"] = map[string]string{"id": "g1-pro-tier", "name": "Google AI Pro"}
			}
			_ = json.NewEncoder(w).Encode(data)
		case "/v1internal:fetchAvailableModels":
			var body struct {
				Project string `json:"project"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": token, "project": body.Project})
		case "/v1internal:retrieveUserQuotaSummary":
			fraction := .25
			if account == "b" {
				fraction = .75
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"buckets": []any{map[string]any{"bucketId": "gemini-5h", "remainingFraction": fraction}}})
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(provider.server.Close)
	target, err := url.Parse(provider.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	provider.client = &http.Client{Transport: runtimeTransport{target: target, base: http.DefaultTransport}, Timeout: 3 * time.Second}
	return provider
}

func runtimeConfig(t *testing.T, provider *runtimeProvider) (config.Config, string) {
	t.Helper()
	for _, key := range []string{"ANTIGRAVITY_ACCESS_TOKEN", "ANTIGRAVITY_REFRESH_TOKEN", "ANTIGRAVITY_OAUTH_CLIENT_ID", "ANTIGRAVITY_OAUTH_CLIENT_SECRET"} {
		old, exists := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if exists {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
	home := t.TempDir()
	cfg := config.Config{Host: "127.0.0.1", APIKey: "runtime-key", AccessToken: "token-a", AccountID: "a", AccountEmail: "a@example.invalid", AccountName: "Account a", DailyEndpoint: provider.server.URL, ProdEndpoint: provider.server.URL, QuotaHistoryPath: filepath.Join(home, "usage.jsonl"), QuotaPollIntervalSeconds: 3600, QuotaHistoryMaxSamples: 20}
	path := filepath.Join(home, "config.json")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	return cfg, path
}

func startRuntime(t *testing.T, cfg config.Config, path string, provider *runtimeProvider) *Runtime {
	t.Helper()
	r, err := newRuntime(context.Background(), cfg, path, provider.client)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r
}

func runtimeRequest(r http.Handler, method, path string, payload any) *httptest.ResponseRecorder {
	var body io.Reader
	if payload != nil {
		data, _ := json.Marshal(payload)
		body = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("x-goog-api-key", "runtime-key")
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	r.ServeHTTP(response, req)
	return response
}

func loginSubmission(t *testing.T, r *Runtime) map[string]string {
	t.Helper()
	response := runtimeRequest(r, http.MethodGet, "/config/login", nil)
	if response.Code != 200 {
		t.Fatalf("begin: %d %s", response.Code, response.Body.String())
	}
	var data struct {
		LoginURL string `json:"login_url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(data.LoginURL)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"code": "code-b", "state": parsed.Query().Get("state")}
}

func completeRuntimeLogin(t *testing.T, r *Runtime) {
	t.Helper()
	response := runtimeRequest(r, http.MethodPost, "/config/login", loginSubmission(t, r))
	if response.Code != 200 {
		t.Fatalf("complete: %d %s", response.Code, response.Body.String())
	}
}

func requireNativeAccount(t *testing.T, r http.Handler, token, project string) {
	t.Helper()
	response := runtimeRequest(r, http.MethodGet, "/models", nil)
	if response.Code != 200 {
		t.Fatalf("native: %d %s", response.Code, response.Body.String())
	}
	var data map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if data["token"] != token || data["project"] != project {
		t.Fatalf("mixed account token/project: %#v", data)
	}
}

func waitQuota(t *testing.T, r *Runtime, fraction float64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response := runtimeRequest(r, http.MethodGet, "/status/limit", nil)
		var snapshot quota.Snapshot
		if response.Code == 200 && json.Unmarshal(response.Body.Bytes(), &snapshot) == nil {
			value := snapshot.Pools.Gemini.FiveHour.RemainingFraction
			if value != nil && *value == fraction {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("expected account quota was not observed")
}

func TestRuntimeLoginReplacesStartupTokenAndPreservesRefresh(t *testing.T) {
	provider := newRuntimeProvider(t)
	cfg, path := runtimeConfig(t, provider)
	r := startRuntime(t, cfg, path, provider)
	waitQuota(t, r, .25)
	submission := loginSubmission(t, r)
	wrong := runtimeRequest(r, http.MethodPost, "/config/login", map[string]string{"code": "code-b", "state": "wrong"})
	if wrong.Code != 400 {
		t.Fatalf("state mismatch accepted: %d", wrong.Code)
	}
	requireNativeAccount(t, r, "token-a", "project-a")
	response := runtimeRequest(r, http.MethodPost, "/config/login", submission)
	if response.Code != 200 {
		t.Fatalf("login: %d %s", response.Code, response.Body.String())
	}
	requireNativeAccount(t, r, "token-b", "project-b")
	waitQuota(t, r, .75)
	account := runtimeRequest(r, http.MethodGet, "/status/account", nil)
	var details AccountDetails
	if err := json.Unmarshal(account.Body.Bytes(), &details); err != nil {
		t.Fatal(err)
	}
	if account.Code != 200 || details.Email != "b@example.invalid" || details.Subscription == nil || details.Subscription.SourceField != "paidTier" || details.Subscription.ID != "g1-pro-tier" {
		t.Fatalf("wrong account/tier: %d %+v", account.Code, details)
	}
	if replay := runtimeRequest(r, http.MethodPost, "/config/login", submission); replay.Code != 409 {
		t.Fatalf("consumed state accepted: %d", replay.Code)
	}
	leaf := r.active.Load().proxy
	leaf.tokenMu.Lock()
	remaining := time.Until(leaf.tokenExpiresAt)
	leaf.tokenExpiresAt = time.Now().Add(-time.Second)
	leaf.tokenMu.Unlock()
	if remaining > 120*time.Second || remaining < 110*time.Second {
		t.Fatalf("reported token lifetime not honored: %v", remaining)
	}
	requireNativeAccount(t, r, "token-b-renewed", "project-b")
	if provider.refreshCalls.Load() != 1 {
		t.Fatalf("refresh count: %d", provider.refreshCalls.Load())
	}
}

func TestRuntimeOldProfileAndProjectCannotOverwriteReplacement(t *testing.T) {
	for _, surface := range []string{"profile", "project"} {
		t.Run(surface, func(t *testing.T) {
			provider := newRuntimeProvider(t)
			arrived, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			gate := func(req *http.Request) {
				if req.Header.Get("Authorization") == "Bearer token-a" {
					once.Do(func() { close(arrived) })
					select {
					case <-release:
					case <-req.Context().Done():
					}
				}
			}
			if surface == "profile" {
				provider.profileGate = gate
			} else {
				provider.projectGate = gate
			}
			cfg, path := runtimeConfig(t, provider)
			if surface == "profile" {
				cfg.AccountEmail = ""
			}
			r := startRuntime(t, cfg, path, provider)
			route := "/models"
			if surface == "profile" {
				route = "/status/account"
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- runtimeRequest(r, http.MethodGet, route, nil) }()
			select {
			case <-arrived:
			case <-time.After(3 * time.Second):
				t.Fatal("old request did not reach provider")
			}
			completeRuntimeLogin(t, r)
			close(release)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("old request did not complete")
			}
			requireNativeAccount(t, r, "token-b", "project-b")
			response := runtimeRequest(r, http.MethodGet, "/status/account", nil)
			var details AccountDetails
			if err := json.Unmarshal(response.Body.Bytes(), &details); err != nil {
				t.Fatal(err)
			}
			if details.Email != "b@example.invalid" || details.Name != "Account b" {
				t.Fatalf("old profile leaked: %+v", details)
			}
		})
	}
}

func TestRuntimeHistoryRestartsInPersistedAccountNamespace(t *testing.T) {
	provider := newRuntimeProvider(t)
	cfg, path := runtimeConfig(t, provider)
	legacy := []byte("legacy history remains unattributed\n")
	if err := os.WriteFile(cfg.QuotaHistoryPath, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	r := startRuntime(t, cfg, path, provider)
	waitQuota(t, r, .25)
	completeRuntimeLogin(t, r)
	waitQuota(t, r, .75)
	if err := r.Close(); err != nil {
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
	if saved.AccountID != "b" || saved.AccountEmail != "b@example.invalid" || saved.AccessToken != "" || saved.RefreshToken != "refresh-b" {
		t.Fatalf("credentials lost identity: %+v", saved)
	}
	restarted := startRuntime(t, saved, path, provider)
	waitQuota(t, restarted, .75)
	for _, account := range []string{"a", "b"} {
		contents, err := os.ReadFile(status.AccountHistoryPath(cfg.QuotaHistoryPath, "google:"+account))
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(bytes.NewReader(contents))
		for {
			var snapshot quota.Snapshot
			if err := decoder.Decode(&snapshot); err == io.EOF {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			expected := .25
			if account == "b" {
				expected = .75
			}
			value := snapshot.Pools.Gemini.FiveHour.RemainingFraction
			if value == nil || *value != expected {
				t.Fatalf("mixed history for %s: %+v", account, snapshot)
			}
		}
	}
	preserved, err := os.ReadFile(cfg.QuotaHistoryPath)
	if err != nil || !bytes.Equal(preserved, legacy) {
		t.Fatal("legacy history changed")
	}
}

func TestRuntimePreparationAndPersistenceFailurePreserveOldAccount(t *testing.T) {
	for _, stage := range []string{"history", "credentials"} {
		t.Run(stage, func(t *testing.T) {
			provider := newRuntimeProvider(t)
			cfg, path := runtimeConfig(t, provider)
			r := startRuntime(t, cfg, path, provider)
			waitQuota(t, r, .25)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "history" {
				if err := os.Mkdir(status.AccountHistoryPath(cfg.QuotaHistoryPath, "google:b"), 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				r.persist = func(string, config.Credentials) error { return errors.New("private disk error must not leak") }
			}
			response := runtimeRequest(r, http.MethodPost, "/config/login", loginSubmission(t, r))
			if response.Code != 500 || strings.Contains(response.Body.String(), "private disk error") {
				t.Fatalf("failure response: %d %s", response.Code, response.Body.String())
			}
			requireNativeAccount(t, r, "token-a", "project-a")
			waitQuota(t, r, .25)
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("failed preparation changed saved account")
			}
		})
	}
}

func TestRuntimeCommittedWarningPublishesMatchingDiskAndLiveAccount(t *testing.T) {
	provider := newRuntimeProvider(t)
	cfg, path := runtimeConfig(t, provider)
	r := startRuntime(t, cfg, path, provider)
	r.persist = func(path string, creds config.Credentials) error {
		if err := config.UpdateCredentials(path, creds); err != nil {
			return err
		}
		return &config.CommitError{Err: errors.New("private fsync cause must not leak")}
	}
	submission := loginSubmission(t, r)
	response := runtimeRequest(r, http.MethodPost, "/config/login", submission)
	var body struct {
		Configured bool `json:"credential_configured"`
		Uncertain  bool `json:"durability_uncertain"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != 500 || !body.Configured || !body.Uncertain || strings.Contains(response.Body.String(), "private fsync cause") {
		t.Fatalf("misleading committed outcome: %d %s", response.Code, response.Body.String())
	}
	requireNativeAccount(t, r, "token-b", "project-b")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved config.Config
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.AccountID != r.active.Load().proxy.cfg.AccountID {
		t.Fatal("disk and live account disagree after committed warning")
	}
	if replay := runtimeRequest(r, http.MethodPost, "/config/login", submission); replay.Code != 409 {
		t.Fatalf("committed state replayed: %d", replay.Code)
	}
}

func TestRuntimeLoginURLCanBeCopiedFromJSON(t *testing.T) {
	provider := newRuntimeProvider(t)
	cfg, path := runtimeConfig(t, provider)
	cfg.AccessToken, cfg.AccountID, cfg.AccountEmail, cfg.AccountName = "", "", "", ""
	r := startRuntime(t, cfg, path, provider)
	response := runtimeRequest(r, http.MethodGet, "/config/login", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("begin: %d %s", response.Code, response.Body.String())
	}
	var data struct {
		LoginURL json.RawMessage `json:"login_url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	// Preserve the literal string a user copies, rather than JSON-decoding it.
	parsed, err := url.Parse(strings.Trim(string(data.LoginURL), `"`))
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if got := query.Get("response_type"); got != "code" {
		t.Fatalf("copied login URL response_type = %q, want code", got)
	}
	if got := query.Get("code_challenge_method"); got != "S256" {
		t.Fatalf("copied login URL code_challenge_method = %q, want S256", got)
	}
}

func TestRuntimeRequiresManagementKeyAndRejectsWithheldBody(t *testing.T) {
	provider := newRuntimeProvider(t)
	cfg, path := runtimeConfig(t, provider)
	cfg.AccessToken, cfg.AccountID, cfg.AccountEmail, cfg.AccountName = "", "", "", ""
	r := startRuntime(t, cfg, path, provider)
	unauthorized := httptest.NewRecorder()
	r.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/config/login", nil))
	if unauthorized.Code != 401 {
		t.Fatalf("missing client key accepted: %d", unauthorized.Code)
	}
	for _, route := range []string{"/models", "/status/account", "/v1beta/models"} {
		if response := runtimeRequest(r, http.MethodGet, route, nil); response.Code != 503 {
			t.Fatalf("unconfigured %s: %d", route, response.Code)
		}
	}
	for range cap(r.slots) {
		r.slots <- struct{}{}
	}
	response := runtimeRequest(r, http.MethodPost, "/v1beta/models/native:generateContent", map[string]any{"contents": []any{}})
	if response.Code != 503 {
		t.Fatalf("missing credentials reached saturated capacity: %d", response.Code)
	}
	for range cap(r.slots) {
		<-r.slots
	}
	server := httptest.NewServer(r)
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.DialTimeout("tcp", parsed.Host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := io.WriteString(connection, "POST /v1beta/models/native:generateContent HTTP/1.1\r\nHost: localhost\r\nx-goog-api-key: runtime-key\r\nContent-Length: 100\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 4096)
	n, err := connection.Read(buffer)
	if err != nil {
		t.Fatalf("missing credentials waited for body: %v", err)
	}
	if !bytes.HasPrefix(buffer[:n], []byte("HTTP/1.1 503")) {
		t.Fatalf("wrong early response: %s", buffer[:n])
	}
	cfg.APIKey = ""
	cfg.QuotaHistoryPath = filepath.Join(t.TempDir(), "usage.jsonl")
	withoutKey := startRuntime(t, cfg, filepath.Join(t.TempDir(), "config.json"), provider)
	if response := runtimeRequest(withoutKey, http.MethodGet, "/config/login", nil); response.Code != 503 {
		t.Fatalf("unconfigured management key: %d", response.Code)
	}
	if provider.requests.Load() != 0 {
		t.Fatal("unconfigured account contacted the OAuth or quota provider")
	}
}

func TestRuntimeSameAccountReloginRetainsDurableHistory(t *testing.T) {
	provider := newRuntimeProvider(t)
	cfg, path := runtimeConfig(t, provider)
	r := startRuntime(t, cfg, path, provider)
	completeRuntimeLogin(t, r)
	waitQuota(t, r, .75)
	history := status.AccountHistoryPath(cfg.QuotaHistoryPath, "google:b")
	before, err := os.ReadFile(history)
	if err != nil {
		t.Fatal(err)
	}
	completeRuntimeLogin(t, r)
	requireNativeAccount(t, r, "token-b", "project-b")
	waitQuota(t, r, .75)
	after, err := os.ReadFile(history)
	if err != nil || !bytes.HasPrefix(after, before) {
		t.Fatal("same-account relogin discarded existing observations")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := os.ReadFile(history)
	if err != nil || !bytes.HasPrefix(reopened, before) {
		t.Fatal("same-account observations were not durable")
	}
}

func TestRuntimeCancellationAfterCredentialCommitStillPublishesMatchingAccount(t *testing.T) {
	provider := newRuntimeProvider(t)
	cfg, path := runtimeConfig(t, provider)
	r := startRuntime(t, cfg, path, provider)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.persist = func(path string, creds config.Credentials) error {
		if err := config.UpdateCredentials(path, creds); err != nil {
			return err
		}
		cancel()
		return nil
	}
	err := r.activate(ctx, oauth.Tokens{AccessToken: "token-b", RefreshToken: "refresh-b", ExpiresIn: 120}, "client", "secret", oauth.UserInfo{ID: "b", Email: "b@example.invalid", Name: "Account b"})
	if err != nil {
		t.Fatal(err)
	}
	requireNativeAccount(t, r, "token-b", "project-b")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved config.Config
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.AccountID != "b" {
		t.Fatal("cancellation left disk and active account inconsistent")
	}
}

func TestRuntimeRapidAccountReplacementKeepsRequestTokenAndProjectTogether(t *testing.T) {
	provider := newRuntimeProvider(t)
	cfg, path := runtimeConfig(t, provider)
	r := startRuntime(t, cfg, path, provider)
	results := make(chan error, 1)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				results <- nil
				return
			default:
			}
			response := runtimeRequest(r, http.MethodGet, "/models", nil)
			var data map[string]string
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &data) != nil {
				results <- errors.New("request failed during replacement")
				return
			}
			account := "a"
			if strings.HasPrefix(data["token"], "token-b") {
				account = "b"
			}
			if data["project"] != "project-"+account {
				results <- errors.New("request mixed token and project from different accounts")
				return
			}
		}
	}()
	for i := range 12 {
		account := "b"
		if i%2 == 1 {
			account = "a"
		}
		if err := r.activate(context.Background(), oauth.Tokens{AccessToken: "token-" + account, ExpiresIn: 120}, "client", "secret", oauth.UserInfo{ID: account, Email: account + "@example.invalid", Name: "Account " + account}); err != nil {
			close(stop)
			<-results
			t.Fatal(err)
		}
	}
	close(stop)
	select {
	case err := <-results:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request loop did not stop")
	}
	requireNativeAccount(t, r, "token-a", "project-a")
}

func TestRuntimeSuccessfulStatusLookupRecoversFailedStartupIdentity(t *testing.T) {
	provider := newRuntimeProvider(t)
	provider.profileFailNext.Store(true)
	cfg, path := runtimeConfig(t, provider)
	cfg.AccountID, cfg.AccountEmail, cfg.AccountName = "", "", ""
	r := startRuntime(t, cfg, path, provider)
	r.background.Wait()
	if r.active.Load().proxy.cfg.AccountID != "" {
		t.Fatal("initial profile failure unexpectedly identified account")
	}
	response := runtimeRequest(r, http.MethodGet, "/status/account", nil)
	if response.Code != 200 {
		t.Fatalf("profile recovery: %d %s", response.Code, response.Body.String())
	}
	waitQuota(t, r, .25)
	requireNativeAccount(t, r, "token-a", "project-a")
	if r.active.Load().proxy.cfg.AccountID != "a" {
		t.Fatal("recovered profile was not promoted for quota polling")
	}
	if _, err := os.Stat(status.AccountHistoryPath(cfg.QuotaHistoryPath, "google:a")); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeIdentityPromotionDoesNotExtendExpiredCachedToken(t *testing.T) {
	provider := newRuntimeProvider(t)
	provider.profileFailNext.Store(true)
	cfg, path := runtimeConfig(t, provider)
	cfg.AccessToken, cfg.RefreshToken = "", "refresh-b"
	cfg.OAuthClientID, cfg.OAuthClientSecret = "client", "secret"
	cfg.AccountID, cfg.AccountEmail, cfg.AccountName = "", "", ""
	r := startRuntime(t, cfg, path, provider)
	r.background.Wait()
	initial := r.active.Load()
	initial.proxy.tokenMu.Lock()
	initial.proxy.cachedToken = "token-b-expired"
	initial.proxy.tokenExpiresAt = time.Now().Add(-time.Second)
	initial.proxy.tokenMu.Unlock()
	before := provider.refreshCalls.Load()
	if err := r.promoteIdentity(context.Background(), initial, oauth.UserInfo{ID: "b", Email: "b@example.invalid", Name: "Account b"}); err != nil {
		t.Fatal(err)
	}
	requireNativeAccount(t, r, "token-b-renewed", "project-b")
	waitQuota(t, r, .75)
	if provider.refreshCalls.Load() != before+1 {
		t.Fatal("expired startup token did not refresh exactly once after promotion")
	}
}

func TestRuntimeLateStartupProfileCannotReplaceCommittedLogin(t *testing.T) {
	provider := newRuntimeProvider(t)
	arrived, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	provider.profileGate = func(req *http.Request) {
		if req.Header.Get("Authorization") == "Bearer token-a" {
			close(arrived)
			select {
			case <-release:
			case <-req.Context().Done():
			}
		}
	}
	cfg, path := runtimeConfig(t, provider)
	cfg.AccountID, cfg.AccountEmail, cfg.AccountName = "", "", ""
	r := startRuntime(t, cfg, path, provider)
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("startup profile did not begin")
	}
	completeRuntimeLogin(t, r)
	releaseOnce.Do(func() { close(release) })
	r.background.Wait()
	requireNativeAccount(t, r, "token-b", "project-b")
	waitQuota(t, r, .75)
	if r.active.Load().proxy.cfg.AccountID != "b" {
		t.Fatal("late startup profile replaced committed account")
	}
}
