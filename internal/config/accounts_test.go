package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"antigravity-proxy/internal/apikey"
	"antigravity-proxy/internal/testutil"
)

func TestValidateAccountID(t *testing.T) {
	valid := []string{"default", "work", "personal", "acc-1", "a", "1", "account-123"}
	for _, id := range valid {
		if err := ValidateAccountID(id); err != nil {
			t.Errorf("ValidateAccountID(%q) = %v, want nil", id, err)
		}
	}

	invalid := []string{
		"",
		"-start",
		"end-",
		"UPPER",
		"has space",
		"has_underscore",
		"a-very-long-account-id-exceeding-thirty-two-chars",
	}
	for _, id := range invalid {
		if err := ValidateAccountID(id); err == nil {
			t.Errorf("ValidateAccountID(%q) = nil, want error", id)
		}
	}
}

func TestDeriveAccountID(t *testing.T) {
	cases := []struct {
		email    string
		existing []string
		want     string
	}{
		{"developer@example.com", nil, "developer"},
		{"DEV.ELOPER@example.com", nil, "dev-eloper"},
		{"first.last+tag@work.co", nil, "first-last-tag"},
		{"user@example.com", []string{"user"}, "user-2"},
		{"user@example.com", []string{"user", "user-2"}, "user-3"},
		{"@empty.com", nil, "account"},
		{"---@empty.com", nil, "account"},
	}
	for _, tc := range cases {
		got := DeriveAccountID(tc.email, tc.existing)
		if got != tc.want {
			t.Errorf("DeriveAccountID(%q, %v) = %q, want %q", tc.email, tc.existing, got, tc.want)
		}
		if err := ValidateAccountID(got); err != nil {
			t.Errorf("DeriveAccountID produced invalid slug %q: %v", got, err)
		}
	}
}

func TestLegacyConfigMigrationOnLoad(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	testutil.UnsetEnv(t, EnvironmentVariables()...)

	configDir := filepath.Join(home, ".config", "antigravity-proxy")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "config.json")

	legacyJSON := `{
  "port": 8080,
  "apiKey": "secret-api-key",
  "refreshToken": "legacy-refresh",
  "accessToken": "legacy-access",
  "accountId": "google-12345",
  "accountEmail": "dev@example.com",
  "accountName": "Developer Name"
}`
	if err := os.WriteFile(configPath, []byte(legacyJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if len(cfg.Accounts) != 1 {
		t.Fatalf("len(cfg.Accounts) = %d, want 1", len(cfg.Accounts))
	}
	acc := cfg.Accounts[0]
	if acc.ID != "default" || acc.AccountEmail != "dev@example.com" || acc.AccountID != "google-12345" {
		t.Fatalf("migrated account = %+v, want default dev@example.com", acc)
	}
	if len(acc.Keys) != 1 {
		t.Fatalf("len(acc.Keys) = %d, want 1", len(acc.Keys))
	}
	if acc.Keys[0].ID != "key_legacy" || !apikey.Match("secret-api-key", acc.Keys[0].Hash) {
		t.Fatalf("migrated key = %+v does not match secret-api-key", acc.Keys[0])
	}
}

func TestUpdateAccountsPersistsAndStripsLegacyFields(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, ".config", "antigravity-proxy")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "config.json")

	legacyJSON := `{
  "port": 8080,
  "apiKey": "old-key",
  "refreshToken": "old-refresh"
}`
	if err := os.WriteFile(configPath, []byte(legacyJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	err := UpdateAccounts(configPath, func(accounts *[]Account) error {
		*accounts = append(*accounts, Account{
			ID:           "work",
			Label:        "Work Account",
			Enabled:      true,
			RefreshToken: "work-refresh",
			Keys: []AccountKey{
				{
					ID:        "key_1",
					Label:     "bifrost",
					Hash:      apikey.Hash("agp_work_secret"),
					Last4:     "cret",
					CreatedAt: time.Now().UTC(),
				},
			},
		})
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateAccounts error = %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)

	// Legacy fields must be stripped
	if strings.Contains(content, `"apiKey"`) || strings.Contains(content, `"refreshToken"`) && !strings.Contains(content, `"accounts"`) {
		t.Fatalf("legacy fields were not stripped from disk: %s", content)
	}

	var parsed struct {
		Accounts []Account `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal updated config error = %v", err)
	}
	if len(parsed.Accounts) != 2 { // "default" from migration + "work"
		t.Fatalf("expected 2 accounts, got %d", len(parsed.Accounts))
	}
}

func TestValidateAccountsUniqueness(t *testing.T) {
	keyHash := apikey.Hash("agp_key_1")

	// Duplicate account ID
	accounts := []Account{
		{ID: "work", Enabled: true},
		{ID: "work", Enabled: true},
	}
	if err := ValidateAccounts(accounts); err == nil {
		t.Fatal("ValidateAccounts allowed duplicate account ID")
	}

	// Duplicate Google account ID
	accounts = []Account{
		{ID: "work", AccountID: "google-1", Enabled: true},
		{ID: "personal", AccountID: "google-1", Enabled: true},
	}
	if err := ValidateAccounts(accounts); err == nil {
		t.Fatal("ValidateAccounts allowed duplicate Google account ID")
	}

	// Duplicate key hash across accounts
	accounts = []Account{
		{
			ID:      "work",
			Enabled: true,
			Keys: []AccountKey{
				{ID: "k1", Hash: keyHash},
			},
		},
		{
			ID:      "personal",
			Enabled: true,
			Keys: []AccountKey{
				{ID: "k2", Hash: keyHash},
			},
		},
	}
	if err := ValidateAccounts(accounts); err == nil {
		t.Fatal("ValidateAccounts allowed duplicate API key hash across accounts")
	}
}

func TestLoopbackMultipleAccountsRequireKeys(t *testing.T) {
	cfg := Config{
		Port:                     8080,
		Host:                     "127.0.0.1",
		QuotaPollIntervalSeconds: 300,
		QuotaHistoryPath:         "/tmp/usage.jsonl",
		QuotaHistoryMaxSamples:   1000,
		ClientVersion:            DefaultClientVersion,
		MaxConcurrentGenerations: 2,
		Accounts: []Account{
			{ID: "acc1", Enabled: true},
			{ID: "acc2", Enabled: true},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() allowed multiple accounts on loopback without API keys")
	}
}

func TestUpdateAdminKey(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, ".config", "antigravity-proxy")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"port":8080}`), 0o600); err != nil {
		t.Fatal(err)
	}

	adminHash := apikey.Hash("my-admin-key")
	if err := UpdateAdminKey(configPath, adminHash); err != nil {
		t.Fatalf("UpdateAdminKey error = %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), adminHash) {
		t.Fatalf("config does not contain admin hash: %s", string(raw))
	}
}
