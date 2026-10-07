package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestValidateCloudEndpointRequiresTLSExceptLoopback(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		wantErr  bool
	}{
		{name: "production TLS", endpoint: "https://cloudcode-pa.googleapis.com"},
		{name: "local IPv4 test server", endpoint: "http://127.0.0.1:19090"},
		{name: "local hostname test server", endpoint: "http://localhost:19090"},
		{name: "remote plaintext endpoint", endpoint: "http://example.com", wantErr: true},
		{name: "unsupported scheme", endpoint: "ftp://example.com", wantErr: true},
		{name: "embedded credentials", endpoint: "https://user:pass@example.com", wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := validateCloudEndpoint("ENDPOINT", test.endpoint)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateCloudEndpoint(%q) error = %v, wantErr %t", test.endpoint, err, test.wantErr)
			}
		})
	}
}

func TestValidateListenAddressRequiresKeyOffLoopback(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		apiKey  string
		wantErr bool
	}{
		{name: "default IPv4", host: "127.0.0.1"},
		{name: "localhost", host: "localhost"},
		{name: "IPv6 loopback", host: "::1"},
		{name: "public listener without key", host: "0.0.0.0", wantErr: true},
		{name: "public listener with key", host: "0.0.0.0", apiKey: "secret"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := validateListenAddress(test.host, test.apiKey)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateListenAddress(%q, API key present %t) error = %v, wantErr %t", test.host, test.apiKey != "", err, test.wantErr)
			}
		})
	}
}

func isolatedLoadHome(t *testing.T) string {
	t.Helper()
	for _, name := range []string{
		"HOST", "PORT", "API_KEY",
		"ANTIGRAVITY_ACCESS_TOKEN", "ANTIGRAVITY_REFRESH_TOKEN",
		"ANTIGRAVITY_PROJECT_ID", "ANTIGRAVITY_DAILY_ENDPOINT", "ANTIGRAVITY_PROD_ENDPOINT",
		"ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS", "ANTIGRAVITY_QUOTA_HISTORY_PATH",
	} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func writeLoadConfig(t *testing.T, home, contents string) string {
	t.Helper()
	path := filepath.Join(home, ".config", "antigravity-proxy", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadQuotaDefaults(t *testing.T) {
	home := isolatedLoadHome(t)
	cfg, path, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuotaPollIntervalSeconds != 300 || DefaultQuotaPollIntervalSeconds != 300 {
		t.Fatalf("default polling interval = %d, constant = %d; want 300", cfg.QuotaPollIntervalSeconds, DefaultQuotaPollIntervalSeconds)
	}
	wantHistory := filepath.Join(home, ".local", "share", "antigravity-proxy", "usage.jsonl")
	if cfg.QuotaHistoryPath != wantHistory {
		t.Fatalf("history path = %q, want %q", cfg.QuotaHistoryPath, wantHistory)
	}
	wantConfig := filepath.Join(home, ".config", "antigravity-proxy", "config.json")
	if path != wantConfig {
		t.Fatalf("config path = %q, want %q", path, wantConfig)
	}
	if cfg.Host != "127.0.0.1" || cfg.Port != 8080 || cfg.APIKey != "" || cfg.AccessToken != "" || cfg.RefreshToken != "" {
		t.Fatal("Load changed listener or credential defaults")
	}
	if cfg.DailyEndpoint != "https://daily-cloudcode-pa.googleapis.com" || cfg.ProdEndpoint != "https://cloudcode-pa.googleapis.com" {
		t.Fatal("Load changed endpoint defaults")
	}
	for _, absent := range []string{filepath.Dir(wantHistory), filepath.Dir(wantConfig)} {
		if _, err := os.Stat(absent); !os.IsNotExist(err) {
			t.Fatalf("Load created or accessed unexpected storage %q: %v", absent, err)
		}
	}
}

func TestLoadQuotaPrecedence(t *testing.T) {
	cases := []struct {
		name        string
		config      string
		envSeconds  string
		envPath     string
		wantSeconds int
		wantPath    string
	}{
		{name: "omitted JSON fields retain defaults", config: `{}`, wantSeconds: 300},
		{name: "JSON overrides defaults", config: `{"quotaPollIntervalSeconds":17,"quotaHistoryPath":"file-history.jsonl"}`, wantSeconds: 17, wantPath: "file-history.jsonl"},
		{name: "environment overrides defaults", config: `{}`, envSeconds: "19", envPath: "env-history.jsonl", wantSeconds: 19, wantPath: "env-history.jsonl"},
		{name: "environment overrides JSON", config: `{"quotaPollIntervalSeconds":17,"quotaHistoryPath":"file-history.jsonl"}`, envSeconds: "23", envPath: "env-history.jsonl", wantSeconds: 23, wantPath: "env-history.jsonl"},
		{name: "environment replaces invalid JSON values", config: `{"quotaPollIntervalSeconds":0,"quotaHistoryPath":""}`, envSeconds: "29", envPath: "env-history.jsonl", wantSeconds: 29, wantPath: "env-history.jsonl"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			home := isolatedLoadHome(t)
			wantConfig := writeLoadConfig(t, home, test.config)
			if test.envSeconds != "" {
				t.Setenv("ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS", test.envSeconds)
			}
			if test.envPath != "" {
				t.Setenv("ANTIGRAVITY_QUOTA_HISTORY_PATH", test.envPath)
			}
			cfg, path, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			wantHistory := test.wantPath
			if wantHistory == "" {
				wantHistory = filepath.Join(home, ".local", "share", "antigravity-proxy", "usage.jsonl")
			}
			if cfg.QuotaPollIntervalSeconds != test.wantSeconds || cfg.QuotaHistoryPath != wantHistory || path != wantConfig {
				t.Fatalf("Load quota = (%d, %q), config path = %q; want (%d, %q), %q", cfg.QuotaPollIntervalSeconds, cfg.QuotaHistoryPath, path, test.wantSeconds, wantHistory, wantConfig)
			}
		})
	}
}

func TestLoadRejectsInvalidQuotaConfiguration(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		envName string
		env     string
		wantErr string
	}{
		{name: "JSON zero", config: `{"quotaPollIntervalSeconds":0}`, wantErr: "quotaPollIntervalSeconds"},
		{name: "JSON negative", config: `{"quotaPollIntervalSeconds":-1}`, wantErr: "quotaPollIntervalSeconds"},
		{name: "JSON fractional", config: `{"quotaPollIntervalSeconds":1.5}`, wantErr: "quotaPollIntervalSeconds"},
		{name: "JSON string", config: `{"quotaPollIntervalSeconds":"300"}`, wantErr: "quotaPollIntervalSeconds"},
		{name: "JSON duration overflow", config: `{"quotaPollIntervalSeconds":9223372037}`, wantErr: "quotaPollIntervalSeconds"},
		{name: "JSON integer overflow", config: `{"quotaPollIntervalSeconds":9223372036854775808}`, wantErr: "quotaPollIntervalSeconds"},
		{name: "JSON empty history", config: `{"quotaHistoryPath":""}`, wantErr: "quotaHistoryPath"},
		{name: "JSON whitespace history", config: `{"quotaHistoryPath":" \t"}`, wantErr: "quotaHistoryPath"},
		{name: "environment zero", envName: "ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS", env: "0", wantErr: "quotaPollIntervalSeconds"},
		{name: "environment negative", envName: "ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS", env: "-1", wantErr: "quotaPollIntervalSeconds"},
		{name: "environment fractional", envName: "ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS", env: "1.5", wantErr: "ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS"},
		{name: "environment noninteger", envName: "ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS", env: "invalid", wantErr: "ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS"},
		{name: "environment empty interval", envName: "ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS", env: "", wantErr: "ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS"},
		{name: "environment duration overflow", envName: "ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS", env: "9223372037", wantErr: "positive integer"},
		{name: "environment integer overflow", envName: "ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS", env: "9223372036854775808", wantErr: "ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS"},
		{name: "environment empty history", envName: "ANTIGRAVITY_QUOTA_HISTORY_PATH", env: "", wantErr: "quotaHistoryPath"},
		{name: "environment whitespace history", envName: "ANTIGRAVITY_QUOTA_HISTORY_PATH", env: " \t", wantErr: "quotaHistoryPath"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			home := isolatedLoadHome(t)
			config := test.config
			if config == "" {
				config = `{"quotaPollIntervalSeconds":17,"quotaHistoryPath":"valid-history.jsonl"}`
			}
			writeLoadConfig(t, home, config)
			if test.envName != "" {
				t.Setenv(test.envName, test.env)
			}
			_, _, err := Load()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Load error = %v, want error containing %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadQuotaMaximumDuration(t *testing.T) {
	isolatedLoadHome(t)
	// A 32-bit int cannot represent the duration limit, so use its own maximum.
	seconds := int(^uint(0) >> 1)
	if strconv.IntSize == 64 {
		maxDurationSeconds := int64(9223372036)
		seconds = int(maxDurationSeconds)
	}
	t.Setenv("ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS", strconv.Itoa(seconds))
	cfg, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuotaPollIntervalSeconds != seconds {
		t.Fatalf("polling interval = %d, want %d", cfg.QuotaPollIntervalSeconds, seconds)
	}
}

func TestLoadQuotaDoesNotOpenHistory(t *testing.T) {
	home := isolatedLoadHome(t)
	parent := filepath.Join(home, "not-a-directory")
	if err := os.WriteFile(parent, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	wantHistory := filepath.Join(parent, "history.jsonl")
	t.Setenv("ANTIGRAVITY_QUOTA_HISTORY_PATH", wantHistory)
	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load must not try opening history: %v", err)
	}
	if cfg.QuotaHistoryPath != wantHistory {
		t.Fatalf("history path = %q, want %q", cfg.QuotaHistoryPath, wantHistory)
	}
	data, err := os.ReadFile(parent)
	if err != nil || string(data) != "unchanged" {
		t.Fatalf("Load modified history parent: %q, %v", data, err)
	}
}

func TestLoadQuotaPreservesExistingOverrides(t *testing.T) {
	home := isolatedLoadHome(t)
	writeLoadConfig(t, home, `{"port":8090,"host":"127.0.0.1","apiKey":"file-key","accessToken":"file-access","refreshToken":"file-refresh","projectId":"file-project","dailyEndpoint":"https://file-daily.example","prodEndpoint":"https://file-prod.example","quotaPollIntervalSeconds":31,"quotaHistoryPath":"file-history.jsonl"}`)
	for name, value := range map[string]string{
		"HOST":                       " 0.0.0.0 ",
		"PORT":                       "9090",
		"API_KEY":                    " env-key ",
		"ANTIGRAVITY_ACCESS_TOKEN":   "env-access",
		"ANTIGRAVITY_REFRESH_TOKEN":  "env-refresh",
		"ANTIGRAVITY_PROJECT_ID":     "env-project",
		"ANTIGRAVITY_DAILY_ENDPOINT": "http://localhost:19090/",
		"ANTIGRAVITY_PROD_ENDPOINT":  "https://env-prod.example/",
	} {
		t.Setenv(name, value)
	}
	cfg, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "0.0.0.0" || cfg.Port != 9090 || cfg.APIKey != "env-key" {
		t.Fatal("quota settings changed listener or client authentication overrides")
	}
	if cfg.AccessToken != "env-access" || cfg.RefreshToken != "env-refresh" || cfg.ProjectID != "env-project" {
		t.Fatal("quota settings changed upstream credential or project overrides")
	}
	if cfg.DailyEndpoint != "http://localhost:19090" || cfg.ProdEndpoint != "https://env-prod.example" {
		t.Fatal("quota settings changed upstream endpoint overrides")
	}
	if cfg.QuotaPollIntervalSeconds != 31 || cfg.QuotaHistoryPath != "file-history.jsonl" {
		t.Fatal("unrelated environment settings changed quota configuration")
	}
}

func TestSaveQuotaConfigurationRoundTrip(t *testing.T) {
	home := isolatedLoadHome(t)
	cfg, path, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.QuotaPollIntervalSeconds = 37
	cfg.QuotaHistoryPath = filepath.Join(home, "history", "quota.jsonl")
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, loadedPath, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded != cfg || loadedPath != path {
		t.Fatal("saved quota configuration did not round-trip")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"quotaPollIntervalSeconds": 37`) || !strings.Contains(string(data), `"quotaHistoryPath":`) {
		t.Fatal("saved configuration omitted quota JSON fields")
	}
	if strings.Contains(string(data), `"accessToken"`) || strings.Contains(string(data), `"refreshToken"`) {
		t.Fatal("quota configuration introduced credential values")
	}
	if _, err := os.Stat(filepath.Dir(cfg.QuotaHistoryPath)); !os.IsNotExist(err) {
		t.Fatalf("Load or Save created history storage: %v", err)
	}
}
