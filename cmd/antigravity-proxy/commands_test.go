package main

import (
	"antigravity-proxy/internal/apikey"
	"antigravity-proxy/internal/config"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setupTestConfig(t *testing.T) (config.Config, string) {
	t.Helper()
	home := t.TempDir()
	configDir := filepath.Join(home, ".config", "antigravity-proxy")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "config.json")

	cfg := config.Config{
		Host: "127.0.0.1",
		Port: 8080,
		Accounts: []config.Account{
			{
				ID:           "work",
				Label:        "Work",
				Enabled:      true,
				AccountEmail: "work@example.com",
				Keys: []config.AccountKey{
					{
						ID:        "key_1",
						Label:     "bifrost",
						Hash:      apikey.Hash("agp_test_secret"),
						Last4:     "cret",
						CreatedAt: time.Now().UTC(),
					},
				},
			},
		},
		QuotaHistoryPath:         filepath.Join(home, "usage.jsonl"),
		QuotaPollIntervalSeconds: 300,
		QuotaHistoryMaxSamples:   100,
		ClientVersion:            config.DefaultClientVersion,
		MaxConcurrentGenerations: 2,
	}

	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	return cfg, configPath
}

func TestCLIAdminKeyRotate(t *testing.T) {
	_, path := setupTestConfig(t)
	var output bytes.Buffer

	if err := runAdminKeyCommand([]string{"rotate"}, path, &output); err != nil {
		t.Fatalf("runAdminKeyCommand error = %v", err)
	}

	outStr := output.String()
	if !strings.Contains(outStr, "Rotated admin API key:") || !strings.Contains(outStr, "agp_") {
		t.Fatalf("unexpected admin-key rotate output: %s", outStr)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"adminKeyHash": "sha256:`) {
		t.Fatalf("config does not contain adminKeyHash: %s", string(raw))
	}
}

func TestCLIAccountsCommands(t *testing.T) {
	cfg, path := setupTestConfig(t)
	var output bytes.Buffer

	// 1. List
	if err := runAccountsCommand([]string{"list"}, cfg, path, &output); err != nil {
		t.Fatalf("accounts list error = %v", err)
	}
	if !strings.Contains(output.String(), "work") || !strings.Contains(output.String(), "enabled") {
		t.Fatalf("accounts list output = %s", output.String())
	}

	// 2. Disable
	output.Reset()
	if err := runAccountsCommand([]string{"disable", "work"}, cfg, path, &output); err != nil {
		t.Fatalf("accounts disable error = %v", err)
	}
	if !strings.Contains(output.String(), "Account \"work\" disabled.") {
		t.Fatalf("accounts disable output = %s", output.String())
	}

	// Verify disk
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"enabled": false`) {
		t.Fatalf("account was not disabled on disk: %s", string(raw))
	}

	// 3. Enable
	output.Reset()
	if err := runAccountsCommand([]string{"enable", "work"}, cfg, path, &output); err != nil {
		t.Fatalf("accounts enable error = %v", err)
	}
	if !strings.Contains(output.String(), "Account \"work\" enabled.") {
		t.Fatalf("accounts enable output = %s", output.String())
	}

	// 4. Remove
	output.Reset()
	if err := runAccountsCommand([]string{"remove", "work"}, cfg, path, &output); err != nil {
		t.Fatalf("accounts remove error = %v", err)
	}
	if !strings.Contains(output.String(), "Account \"work\" removed.") {
		t.Fatalf("accounts remove output = %s", output.String())
	}
}

func TestCLIKeysCommands(t *testing.T) {
	cfg, path := setupTestConfig(t)
	var output bytes.Buffer

	// 1. List
	if err := runKeysCommand([]string{"list", "work"}, cfg, path, &output); err != nil {
		t.Fatalf("keys list error = %v", err)
	}
	if !strings.Contains(output.String(), "key_1") || !strings.Contains(output.String(), "bifrost") {
		t.Fatalf("keys list output = %s", output.String())
	}

	// 2. Create
	output.Reset()
	if err := runKeysCommand([]string{"create", "work", "--label", "new-bifrost"}, cfg, path, &output); err != nil {
		t.Fatalf("keys create error = %v", err)
	}
	outStr := output.String()
	if !strings.Contains(outStr, "Created API key for account \"work\"") || !strings.Contains(outStr, "agp_") {
		t.Fatalf("keys create output = %s", outStr)
	}

	// 3. Revoke
	output.Reset()
	if err := runKeysCommand([]string{"revoke", "work", "key_1"}, cfg, path, &output); err != nil {
		t.Fatalf("keys revoke error = %v", err)
	}
	if !strings.Contains(output.String(), "Key \"key_1\" revoked from account \"work\".") {
		t.Fatalf("keys revoke output = %s", output.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"key_1"`) {
		t.Fatalf("revoked key_1 still in config: %s", string(raw))
	}
}
