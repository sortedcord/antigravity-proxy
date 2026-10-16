package proxy

import (
	"antigravity-proxy/internal/apikey"
	"antigravity-proxy/internal/config"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newMultiAccountTestServer(t *testing.T) (*Runtime, string, string, string) {
	t.Helper()
	home := t.TempDir()
	configPath := filepath.Join(home, "config.json")

	adminKey := "agp_admin_secret_key_1234567890"
	workKey := "agp_work_secret_key_1234567890"
	personalKey := "agp_personal_secret_key_1234567890"

	cfg := config.Config{
		Host:         "127.0.0.1",
		Port:         8080,
		AdminAPIKey:  adminKey,
		AdminKeyHash: apikey.Hash(adminKey),
		Accounts: []config.Account{
			{
				ID:           "work",
				Label:        "Work Account",
				Enabled:      true,
				AccessToken:  "token-work",
				AccountID:    "google-work",
				AccountEmail: "work@example.com",
				AccountName:  "Work User",
				Keys: []config.AccountKey{
					{
						ID:        "key_work_1",
						Label:     "work-key",
						Hash:      apikey.Hash(workKey),
						Last4:     apikey.Last4(workKey),
						CreatedAt: time.Now().UTC(),
					},
				},
			},
			{
				ID:           "personal",
				Label:        "Personal Account",
				Enabled:      true,
				AccessToken:  "token-personal",
				AccountID:    "google-personal",
				AccountEmail: "personal@example.com",
				AccountName:  "Personal User",
				Keys: []config.AccountKey{
					{
						ID:        "key_personal_1",
						Label:     "personal-key",
						Hash:      apikey.Hash(personalKey),
						Last4:     apikey.Last4(personalKey),
						CreatedAt: time.Now().UTC(),
					},
				},
			},
		},
		QuotaHistoryPath:         filepath.Join(home, "usage.jsonl"),
		QuotaPollIntervalSeconds: 3600,
		QuotaHistoryMaxSamples:   100,
	}

	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}

	r, err := NewRuntime(context.Background(), cfg, configPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })

	return r, adminKey, workKey, personalKey
}

func TestMultiAccountKeyDispatch(t *testing.T) {
	r, _, workKey, personalKey := newMultiAccountTestServer(t)

	// 1. Request with workKey gets work account details
	req := httptest.NewRequest(http.MethodGet, "/status/account", nil)
	req.Header.Set("x-goog-api-key", workKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("workKey /status/account code = %d, want 200", w.Code)
	}
	var workDetails AccountDetails
	if err := json.Unmarshal(w.Body.Bytes(), &workDetails); err != nil {
		t.Fatal(err)
	}
	if workDetails.Email != "work@example.com" {
		t.Fatalf("workKey email = %q, want work@example.com", workDetails.Email)
	}

	// 2. Request with personalKey gets personal account details
	req = httptest.NewRequest(http.MethodGet, "/status/account", nil)
	req.Header.Set("x-goog-api-key", personalKey)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("personalKey /status/account code = %d, want 200", w.Code)
	}
	var personalDetails AccountDetails
	if err := json.Unmarshal(w.Body.Bytes(), &personalDetails); err != nil {
		t.Fatal(err)
	}
	if personalDetails.Email != "personal@example.com" {
		t.Fatalf("personalKey email = %q, want personal@example.com", personalDetails.Email)
	}

	// 3. Request with unknown key returns 401
	req = httptest.NewRequest(http.MethodGet, "/status/account", nil)
	req.Header.Set("x-goog-api-key", "agp_unknown_key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key code = %d, want 401", w.Code)
	}

	// 4. Request to /v1beta/ with unknown key returns Gemini-formatted 401
	req = httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	req.Header.Set("x-goog-api-key", "agp_unknown_key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key /v1beta/models code = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid API key") {
		t.Fatalf("unknown key response = %s, want invalid API key", w.Body.String())
	}
}

func TestDisabledAccountReturnsForbidden(t *testing.T) {
	r, adminKey, workKey, _ := newMultiAccountTestServer(t)

	// Disable work account via PATCH
	body := `{"enabled": false}`
	req := httptest.NewRequest(http.MethodPatch, "/admin/accounts/work", strings.NewReader(body))
	req.Header.Set("x-api-key", adminKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("PATCH /admin/accounts/work code = %d, want 200", w.Code)
	}

	// Request with workKey must return 403
	req = httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	req.Header.Set("x-goog-api-key", workKey)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("disabled account code = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "account is disabled") {
		t.Fatalf("disabled account response = %s, want account is disabled", w.Body.String())
	}
}

func TestAdminAuthenticationRules(t *testing.T) {
	r, adminKey, workKey, _ := newMultiAccountTestServer(t)

	// 1. Missing key returns 401
	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing key code = %d, want 401", w.Code)
	}

	// 2. Account key on admin endpoint returns 401
	req = httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	req.Header.Set("x-api-key", workKey)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("account key on admin endpoint code = %d, want 401", w.Code)
	}

	// 3. Admin key in query param is rejected (allowQuery=false)
	req = httptest.NewRequest(http.MethodGet, "/admin/accounts?key="+adminKey, nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("admin key in query param code = %d, want 401", w.Code)
	}

	// 4. Admin key in header succeeds
	req = httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	req.Header.Set("x-api-key", adminKey)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("valid admin key code = %d, want 200", w.Code)
	}

	// 5. Admin key on data routes returns 401 (not an account key)
	req = httptest.NewRequest(http.MethodGet, "/status/account", nil)
	req.Header.Set("x-goog-api-key", adminKey)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("admin key on data route code = %d, want 401", w.Code)
	}
}

func TestAdminAccountsCRUDAndKeyLifecycle(t *testing.T) {
	r, adminKey, _, _ := newMultiAccountTestServer(t)

	// 1. List accounts
	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	req.Header.Set("x-api-key", adminKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/accounts code = %d, want 200", w.Code)
	}
	var accounts []struct {
		ID       string `json:"id"`
		KeyCount int    `json:"key_count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &accounts); err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 {
		t.Fatalf("len(accounts) = %d, want 2", len(accounts))
	}

	// 2. Get single account
	req = httptest.NewRequest(http.MethodGet, "/admin/accounts/work", nil)
	req.Header.Set("x-api-key", adminKey)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/accounts/work code = %d, want 200", w.Code)
	}

	// 3. Create a new key for work account
	req = httptest.NewRequest(http.MethodPost, "/admin/accounts/work/keys", strings.NewReader(`{"label": "test-bifrost-key"}`))
	req.Header.Set("x-api-key", adminKey)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("POST /admin/accounts/work/keys code = %d, want 201", w.Code)
	}
	var newKeyResp struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Key   string `json:"key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &newKeyResp); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(newKeyResp.Key, "agp_") || newKeyResp.ID == "" {
		t.Fatalf("invalid newly minted key: %+v", newKeyResp)
	}

	// 4. Newly minted key works immediately on data routes
	req = httptest.NewRequest(http.MethodGet, "/status/account", nil)
	req.Header.Set("x-goog-api-key", newKeyResp.Key)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("new key /status/account code = %d, want 200", w.Code)
	}

	// 5. List keys for work account
	req = httptest.NewRequest(http.MethodGet, "/admin/accounts/work/keys", nil)
	req.Header.Set("x-api-key", adminKey)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var keys []apikey.KeyMetadata
	if err := json.Unmarshal(w.Body.Bytes(), &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 { // initial + newly minted
		t.Fatalf("work keys count = %d, want 2", len(keys))
	}

	// 6. Delete (revoke) the new key
	req = httptest.NewRequest(http.MethodDelete, "/admin/accounts/work/keys/"+newKeyResp.ID, nil)
	req.Header.Set("x-api-key", adminKey)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE key code = %d, want 204", w.Code)
	}

	// 7. Revoked key now returns 401
	req = httptest.NewRequest(http.MethodGet, "/status/account", nil)
	req.Header.Set("x-goog-api-key", newKeyResp.Key)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key code = %d, want 401", w.Code)
	}

	// 8. Delete personal account
	req = httptest.NewRequest(http.MethodDelete, "/admin/accounts/personal", nil)
	req.Header.Set("x-api-key", adminKey)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE account code = %d, want 204", w.Code)
	}

	// 9. Deleted account personal returns 404
	req = httptest.NewRequest(http.MethodGet, "/admin/accounts/personal", nil)
	req.Header.Set("x-api-key", adminKey)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("GET deleted account code = %d, want 404", w.Code)
	}
}

func TestLegacyKeyCannotAccessAdmin(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.json")
	cfg := config.Config{
		Host:                     "127.0.0.1",
		APIKey:                   "data-secret",
		QuotaHistoryPath:         filepath.Join(home, "usage.jsonl"),
		QuotaPollIntervalSeconds: 3600,
		QuotaHistoryMaxSamples:   10,
	}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}

	// 1. With unconfigured ADMIN_API_KEY, legacy APIKey must get 503
	r, err := NewRuntime(context.Background(), cfg, path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	req.Header.Set("x-api-key", "data-secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured admin got %d, want 503", w.Code)
	}
	_ = r.Close()

	// 2. With configured ADMIN_API_KEY, data-secret must get 401
	cfg.AdminAPIKey = "admin-secret"
	cfg.AdminKeyHash = apikey.Hash("admin-secret")
	r2, err := NewRuntime(context.Background(), cfg, path)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()

	req = httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	req.Header.Set("x-api-key", "data-secret")
	w = httptest.NewRecorder()
	r2.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("data key accessing admin got %d, want 401", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	req.Header.Set("x-api-key", "admin-secret")
	w = httptest.NewRecorder()
	r2.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("admin key accessing admin got %d, want 200", w.Code)
	}
}

func TestEnvAPIKeySurvivesAdminMutation(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.json")
	disk := config.Config{
		Host: "127.0.0.1",
		Accounts: []config.Account{
			{
				ID:      "default",
				Enabled: true,
			},
		},
		AdminAPIKey:              "admin-secret",
		AdminKeyHash:             apikey.Hash("admin-secret"),
		QuotaHistoryPath:         filepath.Join(home, "usage.jsonl"),
		QuotaPollIntervalSeconds: 3600,
		QuotaHistoryMaxSamples:   10,
	}
	if err := config.Save(path, disk); err != nil {
		t.Fatal(err)
	}

	cfg := disk
	t.Setenv("API_KEY", "env-secret")
	cfg.APIKey = "env-secret"
	cfg.Env.HasAPIKey = true
	cfg.Env.APIKey = "env-secret"
	r, err := NewRuntime(context.Background(), cfg, path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Calling /status/account with env key returns 503 (credentials unconfigured), NOT 401
	req := httptest.NewRequest(http.MethodGet, "/status/account", nil)
	req.Header.Set("x-api-key", "env-secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("env key before mutation returned 401 Unauthorized")
	}

	// Perform admin PATCH mutation
	patchReq := httptest.NewRequest(http.MethodPatch, "/admin/accounts/default", strings.NewReader(`{"label":"Updated Default"}`))
	patchReq.Header.Set("x-api-key", "admin-secret")
	patchW := httptest.NewRecorder()
	r.ServeHTTP(patchW, patchReq)
	if patchW.Code != http.StatusOK {
		t.Fatalf("PATCH default account failed: %d", patchW.Code)
	}

	// Calling /status/account with env key must STILL NOT return 401
	req = httptest.NewRequest(http.MethodGet, "/status/account", nil)
	req.Header.Set("x-api-key", "env-secret")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("env key after admin mutation returned 401 Unauthorized; env key was lost!")
	}
}

func TestEnvDefinedAccountIsImmutable(t *testing.T) {
	t.Setenv("ANTIGRAVITY_ACCESS_TOKEN", "token-override")
	home := t.TempDir()
	path := filepath.Join(home, "config.json")
	cfg := config.Config{
		Host:                     "127.0.0.1",
		AdminAPIKey:              "admin-secret",
		AdminKeyHash:             apikey.Hash("admin-secret"),
		QuotaHistoryPath:         filepath.Join(home, "usage.jsonl"),
		QuotaPollIntervalSeconds: 3600,
		QuotaHistoryMaxSamples:   10,
	}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}

	r, err := NewRuntime(context.Background(), cfg, path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// 1. PATCH returns 409
	req := httptest.NewRequest(http.MethodPatch, "/admin/accounts/default", strings.NewReader(`{"label":"New"}`))
	req.Header.Set("x-api-key", "admin-secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("PATCH env account got %d, want 409 Conflict", w.Code)
	}

	// 2. DELETE returns 409
	req = httptest.NewRequest(http.MethodDelete, "/admin/accounts/default", nil)
	req.Header.Set("x-api-key", "admin-secret")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("DELETE env account got %d, want 409 Conflict", w.Code)
	}

	// 3. Keys create returns 409
	req = httptest.NewRequest(http.MethodPost, "/admin/accounts/default/keys", strings.NewReader(`{"label":"k"}`))
	req.Header.Set("x-api-key", "admin-secret")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("POST keys on env account got %d, want 409 Conflict", w.Code)
	}

	// 4. Login session targeting default returns 409
	req = httptest.NewRequest(http.MethodPost, "/admin/login-sessions", strings.NewReader(`{"account":"default"}`))
	req.Header.Set("x-api-key", "admin-secret")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("POST login session targeting env account got %d, want 409 Conflict", w.Code)
	}
}

func TestAdminMalformedJSONAndCallbackUserRejection(t *testing.T) {
	r, adminKey, _, _ := newMultiAccountTestServer(t)

	// 1. Unknown fields on login session creation returns 400
	req := httptest.NewRequest(http.MethodPost, "/admin/login-sessions", strings.NewReader(`{"account":"work","extra_field":123}`))
	req.Header.Set("x-api-key", adminKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field in login session creation got %d, want 400", w.Code)
	}

	// 2. Unknown fields on key creation returns 400
	req = httptest.NewRequest(http.MethodPost, "/admin/accounts/work/keys", strings.NewReader(`{"label":"k","extra":true}`))
	req.Header.Set("x-api-key", adminKey)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field in key creation got %d, want 400", w.Code)
	}

	// 3. Callback URL with userinfo is rejected with 400
	req = httptest.NewRequest(http.MethodPost, "/admin/login-sessions/ls_123/complete", strings.NewReader(`{"url":"http://user:pass@127.0.0.1:51121/oauth-callback"}`))
	req.Header.Set("x-api-key", adminKey)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("callback URL with user info got %d, want 400", w.Code)
	}
}

func TestAdminQuotaOverviewEmptyArray(t *testing.T) {
	r, adminKey, _, _ := newMultiAccountTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/admin/quota", nil)
	req.Header.Set("x-api-key", adminKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/quota code = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"accounts":`) {
		t.Fatalf("GET /admin/quota response = %s, missing accounts", w.Body.String())
	}
}

func TestEnvKeyRoutingStableAcrossAdminReload(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.json")
	workKey := "agp_work_key_12345678901234567890123456789012345"
	disk := config.Config{
		Host: "127.0.0.1",
		Accounts: []config.Account{
			{
				ID:           "work",
				Label:        "Work",
				Enabled:      true,
				AccessToken:  "work-token",
				AccountID:    "work-id",
				AccountEmail: "work@example.com",
				Keys: []config.AccountKey{
					{
						ID:        "key_work",
						Label:     "work",
						Hash:      apikey.Hash(workKey),
						Last4:     apikey.Last4(workKey),
						CreatedAt: time.Now().UTC(),
					},
				},
			},
		},
		AdminAPIKey:              "admin-secret",
		AdminKeyHash:             apikey.Hash("admin-secret"),
		QuotaHistoryPath:         filepath.Join(home, "usage.jsonl"),
		QuotaPollIntervalSeconds: 3600,
		QuotaHistoryMaxSamples:   10,
	}
	if err := config.Save(path, disk); err != nil {
		t.Fatal(err)
	}

	cfg := disk
	t.Setenv("API_KEY", "env-secret")
	cfg.APIKey = "env-secret"
	cfg.Env.HasAPIKey = true
	cfg.Env.APIKey = "env-secret"
	r, err := NewRuntime(context.Background(), cfg, path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Before reload: env-secret must route to "default" (unconfigured 503, NOT work's configured account)
	req := httptest.NewRequest(http.MethodGet, "/status/account", nil)
	req.Header.Set("x-api-key", "env-secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("before reload: env key routed to %d, want 503 (default unconfigured)", w.Code)
	}

	// PATCH work account
	patchReq := httptest.NewRequest(http.MethodPatch, "/admin/accounts/work", strings.NewReader(`{"label":"New Work"}`))
	patchReq.Header.Set("x-api-key", "admin-secret")
	patchW := httptest.NewRecorder()
	r.ServeHTTP(patchW, patchReq)
	if patchW.Code != http.StatusOK {
		t.Fatalf("PATCH work failed: %d %s", patchW.Code, patchW.Body.String())
	}

	// After reload: env-secret must STILL route to "default" (503), NEVER to "work" (which would be 200 or return work@example.com)
	req = httptest.NewRequest(http.MethodGet, "/status/account", nil)
	req.Header.Set("x-api-key", "env-secret")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("after reload: env key routed to %d (%s), want 503; key must not switch to work account!", w.Code, w.Body.String())
	}
}

func TestAdminMutationPermissionErrorReturns500(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.json")
	disk := config.Config{
		Host: "127.0.0.1",
		Accounts: []config.Account{
			{
				ID:      "work",
				Label:   "Work",
				Enabled: true,
			},
		},
		AdminAPIKey:              "admin-secret",
		AdminKeyHash:             apikey.Hash("admin-secret"),
		QuotaHistoryPath:         filepath.Join(home, "usage.jsonl"),
		QuotaPollIntervalSeconds: 3600,
		QuotaHistoryMaxSamples:   10,
	}
	if err := config.Save(path, disk); err != nil {
		t.Fatal(err)
	}

	r, err := NewRuntime(context.Background(), disk, path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Make config group-readable so permissions check fails
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPatch, "/admin/accounts/work", strings.NewReader(`{"label":"Hacked"}`))
	req.Header.Set("x-api-key", "admin-secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("PATCH on unreadable config got %d (%s), want 500", w.Code, w.Body.String())
	}
}

func TestLegacyOAuthPairNotReappliedOnReload(t *testing.T) {
	provider := newRuntimeProvider(t)
	cfg, path := runtimeConfig(t, provider)
	cfg.OAuthClientID, cfg.OAuthClientSecret = "legacy-id", "legacy-secret"
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	r := startRuntime(t, cfg, path, provider)
	completeRuntimeLogin(t, r)

	disk, err := config.ReadDiskAccounts(path)
	if err != nil {
		t.Fatal(err)
	}
	newClientID := disk[0].OAuthClientID
	if newClientID == "legacy-id" || newClientID == "" {
		t.Fatalf("expected new OAuth client ID after login, got %q", newClientID)
	}

	// Trigger administrative reload via PATCH
	patchRes := runtimeRequest(r, http.MethodPatch, "/admin/accounts/default", map[string]string{"label": "New Label"})
	if patchRes.Code != http.StatusOK {
		t.Fatalf("PATCH default failed: %d %s", patchRes.Code, patchRes.Body.String())
	}

	reloadedClientID := r.registry.Load().accounts["default"].proxy.cfg.OAuthClientID
	if reloadedClientID != newClientID {
		t.Fatalf("reload reverted OAuth client to %q, want %q", reloadedClientID, newClientID)
	}
}

func TestRevokedLegacyKeyStaysRevoked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, k := range config.EnvironmentVariables() {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("ADMIN_API_KEY", "admin")
	path := filepath.Join(home, ".config", "antigravity-proxy", "config.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	legacy := `{"host":"127.0.0.1","apiKey":"legacy-secret","accessToken":"tok","accountId":"g1","accountEmail":"a@example.invalid","quotaHistoryPath":"` + filepath.Join(home, "u.jsonl") + `"}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, p, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRuntime(context.Background(), cfg, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Initial request with legacy-secret should work (200 on /status/account)
	req := httptest.NewRequest(http.MethodGet, "/status/account", nil)
	req.Header.Set("x-api-key", "legacy-secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("before revoke: legacy key code = %d, want 200", w.Code)
	}

	// Revoke key_legacy via DELETE
	delReq := httptest.NewRequest(http.MethodDelete, "/admin/accounts/default/keys/key_legacy", nil)
	delReq.Header.Set("x-api-key", "admin")
	delW := httptest.NewRecorder()
	r.ServeHTTP(delW, delReq)
	if delW.Code != http.StatusNoContent {
		t.Fatalf("DELETE key_legacy code = %d, want 204", delW.Code)
	}

	// After revoke: legacy-secret must return 401 Unauthorized!
	req = httptest.NewRequest(http.MethodGet, "/status/account", nil)
	req.Header.Set("x-api-key", "legacy-secret")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("after revoke: legacy key still accepted: %d", w.Code)
	}

	// Restart runtime: legacy-secret must STILL return 401 Unauthorized!
	_ = r.Close()
	cfg2, p2, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	r2, err := NewRuntime(context.Background(), cfg2, p2)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()

	req = httptest.NewRequest(http.MethodGet, "/status/account", nil)
	req.Header.Set("x-api-key", "legacy-secret")
	w = httptest.NewRecorder()
	r2.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("after restart: legacy key still accepted: %d", w.Code)
	}
}

func TestPublishFailureAfterCommitSurfacesKey(t *testing.T) {
	provider := newRuntimeProvider(t)
	cfg, path := runtimeConfig(t, provider)
	r := startRuntime(t, cfg, path, provider)

	// Simulate publish failure after persist succeeds by making config file group-readable
	// right before publish reads it from disk.
	origPersist := r.persist
	r.persist = func(p string, fn func(*[]config.Account) error) error {
		err := origPersist(p, fn)
		_ = os.Chmod(p, 0o644)
		return err
	}

	// Create a login session requesting an issued key
	beginRes := runtimeRequest(r, http.MethodPost, "/admin/login-sessions", map[string]any{
		"account":   "default",
		"replace":   true,
		"issue_key": "bifrost",
	})
	if beginRes.Code != http.StatusOK {
		t.Fatalf("begin session failed: %d %s", beginRes.Code, beginRes.Body.String())
	}
	var sessionData struct {
		ID       string `json:"id"`
		LoginURL string `json:"login_url"`
	}
	if err := json.Unmarshal(beginRes.Body.Bytes(), &sessionData); err != nil {
		t.Fatal(err)
	}
	parsedURL, _ := url.Parse(sessionData.LoginURL)
	state := parsedURL.Query().Get("state")

	resp := runtimeRequest(r, http.MethodPost, "/admin/login-sessions/"+sessionData.ID+"/complete", map[string]string{
		"code":  "code-b",
		"state": state,
	})
	_ = os.Chmod(path, 0o600)

	// The status should be 500 (CommitError / durability uncertain warning)
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("complete response code = %d, want 500 (durability warning)", resp.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["credential_configured"] != true {
		t.Fatalf("expected credential_configured = true, got %v", body["credential_configured"])
	}
	keyObj, ok := body["key"].(map[string]any)
	if !ok || keyObj["secret"] == "" {
		t.Fatalf("expected issued key in response even on publish warning, got: %+v", body)
	}
	if !strings.HasPrefix(keyObj["secret"].(string), "agp_") {
		t.Fatalf("expected valid agp_ secret key, got: %v", keyObj["secret"])
	}
}

func TestDeleteLastKeyMultiAccountLoopbackReturns409(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, k := range config.EnvironmentVariables() {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("ADMIN_API_KEY", "admin")
	path := filepath.Join(home, ".config", "antigravity-proxy", "config.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	keySecret := "agp_test_secret_123456789012345678901234567890"
	disk := config.Config{
		Host: "127.0.0.1",
		Accounts: []config.Account{
			{
				ID:          "a",
				Enabled:     true,
				AccessToken: "tok-a",
				Keys: []config.AccountKey{
					{
						ID:        "ka",
						Hash:      apikey.Hash(keySecret),
						Last4:     "7890",
						CreatedAt: time.Now().UTC(),
					},
				},
			},
			{
				ID:          "b",
				Enabled:     true,
				AccessToken: "tok-b",
			},
		},
		QuotaHistoryPath:         filepath.Join(home, "u.jsonl"),
		QuotaPollIntervalSeconds: 3600,
		QuotaHistoryMaxSamples:   10,
	}
	if err := config.Save(path, disk); err != nil {
		t.Fatal(err)
	}
	cfg, p, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRuntime(context.Background(), cfg, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Deleting the only key across 2 accounts on loopback must return 409 Conflict
	delReq := httptest.NewRequest(http.MethodDelete, "/admin/accounts/a/keys/ka", nil)
	delReq.Header.Set("x-api-key", "admin")
	delW := httptest.NewRecorder()
	r.ServeHTTP(delW, delReq)

	if delW.Code != http.StatusConflict {
		t.Fatalf("delete last key code = %d (%s), want 409 Conflict", delW.Code, delW.Body.String())
	}
	if !strings.Contains(delW.Body.String(), "API keys are required when multiple accounts are configured") {
		t.Fatalf("unexpected error message: %s", delW.Body.String())
	}

	// Verify key still exists and config loads cleanly
	loaded, _, err := config.Load()
	if err != nil {
		t.Fatalf("config became unloadable: %v", err)
	}
	if len(loaded.Accounts[0].Keys) != 1 {
		t.Fatalf("key was removed despite rejection: %+v", loaded.Accounts[0].Keys)
	}
}

// Regression test for #7: a per-account key must authenticate even when the
// global API_KEY environment variable is configured.
func TestPerAccountKeyAuthenticatesWhenEnvAPIKeySet(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.json")
	disk := config.Config{
		Host: "127.0.0.1",
		Accounts: []config.Account{
			{ID: "default", Enabled: true},
			{
				ID:      "acc_second",
				Enabled: true,
				Keys: []config.AccountKey{{
					ID:    "key_second",
					Hash:  apikey.Hash("account-secret"),
					Last4: apikey.Last4("account-secret"),
				}},
			},
		},
		AdminAPIKey:              "admin-secret",
		AdminKeyHash:             apikey.Hash("admin-secret"),
		QuotaHistoryPath:         filepath.Join(home, "usage.jsonl"),
		QuotaPollIntervalSeconds: 3600,
		QuotaHistoryMaxSamples:   10,
	}
	if err := config.Save(path, disk); err != nil {
		t.Fatal(err)
	}

	cfg := disk
	t.Setenv("API_KEY", "env-secret")
	cfg.APIKey = "env-secret"
	cfg.Env.HasAPIKey = true
	cfg.Env.APIKey = "env-secret"
	r, err := NewRuntime(context.Background(), cfg, path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	for _, tc := range []struct {
		name, key string
		wantAuth  bool
	}{
		{name: "per-account key", key: "account-secret", wantAuth: true},
		{name: "env key", key: "env-secret", wantAuth: true},
		{name: "unknown key", key: "wrong-secret", wantAuth: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
			req.Header.Set("x-goog-api-key", tc.key)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			gotAuth := w.Code != http.StatusUnauthorized
			if gotAuth != tc.wantAuth {
				t.Fatalf("key %q: status %d, want authenticated=%v; body=%s", tc.key, w.Code, tc.wantAuth, w.Body.String())
			}
		})
	}
}
