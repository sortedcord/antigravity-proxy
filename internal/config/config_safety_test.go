package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestLoadRejectsUnknownAndTrailingJSON(t *testing.T) {
	for _, contents := range []string{
		`{"quotaHistoryMaxSample":4}`,
		`{"host":"127.0.0.1"} {}`,
		`{} true`,
		`{} trailing`,
		`null`,
		`[]`,
	} {
		t.Run(contents, func(t *testing.T) {
			home := isolatedLoadHome(t)
			writeLoadConfig(t, home, contents)
			if _, _, err := Load(); err == nil {
				t.Fatal("Load accepted invalid or unknown configuration")
			}
		})
	}
}

func TestConfigRejectsDuplicateTopLevelFieldsWithoutChangingDisk(t *testing.T) {
	for _, field := range []struct {
		key, first, second string
	}{
		{"host", `"127.0.0.1"`, `"localhost"`},
		{"projectId", `"first-project"`, `"second-project"`},
		{"quotaPollIntervalSeconds", `31`, `47`},
		{"accessToken", `"first-secret-marker"`, `"second-secret-marker"`},
		{"refreshToken", `"first-secret-marker"`, `"second-secret-marker"`},
		{"oauthClientId", `"first-secret-marker"`, `"second-secret-marker"`},
		{"oauthClientSecret", `"first-secret-marker"`, `"second-secret-marker"`},
	} {
		for _, duplicate := range []struct {
			name, key, value string
		}{
			{"exact", field.key, field.second},
			{"case", strings.ToUpper(field.key), field.second},
			{"escaped", fmt.Sprintf(`\u%04x%s`, field.key[0], field.key[1:]), field.second},
			{"escaped case", fmt.Sprintf(`\u%04x%s`, strings.ToUpper(field.key)[0], strings.ToUpper(field.key)[1:]), field.second},
			{"equal exact values", field.key, field.first},
			{"equal case values", strings.ToUpper(field.key), field.first},
		} {
			t.Run(field.key+"/"+duplicate.name, func(t *testing.T) {
				home := isolatedLoadHome(t)
				contents := fmt.Sprintf(`{"%s":%s,"%s":%s}`, field.key, field.first, duplicate.key, duplicate.value)
				path := writeLoadConfig(t, home, contents)
				for _, operation := range []struct {
					name string
					run  func() error
				}{
					{"Load", func() error {
						_, _, err := Load()
						return err
					}},
					{"UpdateCredentials", func() error {
						return UpdateCredentials(path, "new-access", "new-refresh", "new-id", "new-secret")
					}},
				} {
					t.Run(operation.name, func(t *testing.T) {
						err := operation.run()
						if err == nil || !strings.Contains(err.Error(), "duplicate") {
							t.Fatal("ambiguous config was not rejected as duplicate fields")
						}
						for _, secret := range []string{"first-secret-marker", "second-secret-marker", "new-access", "new-refresh", "new-id", "new-secret"} {
							if strings.Contains(err.Error(), secret) {
								t.Fatal("duplicate config error exposed a credential value")
							}
						}
						data, err := os.ReadFile(path)
						if err != nil || string(data) != contents {
							t.Fatal("rejected config operation changed disk settings", err)
						}
					})
				}
			})
		}
	}
}

func TestConfigAcceptsUniqueCaseAndEscapedAliases(t *testing.T) {
	home := isolatedLoadHome(t)
	path := writeLoadConfig(t, home, `{"HOST":"localhost","ProjectID":"keep","QUOTAPOLLINTERVALSECONDS":37,"ACCESS\u0054OKEN":"old-access","REFRESHTOKEN":"old-refresh","OAuthClientID":"old-id","OAUTHCLIENTSECRET":"old-secret"}`)
	cfg, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "localhost" || cfg.ProjectID != "keep" || cfg.QuotaPollIntervalSeconds != 37 || cfg.AccessToken != "old-access" || cfg.RefreshToken != "old-refresh" || cfg.OAuthClientID != "old-id" || cfg.OAuthClientSecret != "old-secret" {
		t.Fatal("unique aliases did not load their saved values")
	}
	if err := UpdateCredentials(path, "new-access", "new-refresh", "new-id", "new-secret"); err != nil {
		t.Fatal(err)
	}
	cfg, _, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "localhost" || cfg.ProjectID != "keep" || cfg.QuotaPollIntervalSeconds != 37 || cfg.AccessToken != "new-access" || cfg.RefreshToken != "new-refresh" || cfg.OAuthClientID != "new-id" || cfg.OAuthClientSecret != "new-secret" {
		t.Fatal("credential update changed unrelated effective values or retained old credentials")
	}
}

func TestLoadRequiresPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses directory ACLs rather than Unix mode bits")
	}
	for _, mode := range []os.FileMode{0600, 0400, 0640, 0604, 0644, 0666} {
		t.Run(fmt.Sprintf("%o", mode), func(t *testing.T) {
			home := isolatedLoadHome(t)
			path := writeLoadConfig(t, home, `{}`)
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			_, _, err := Load()
			if (err != nil) != (mode&0077 != 0) {
				t.Fatalf("Load mode %o returned %v", mode, err)
			}
		})
	}
}

func TestLoadResourceOverrides(t *testing.T) {
	cases := []struct {
		name           string
		contents       string
		environment    bool
		maxSamples     int
		maxGenerations int
		version        string
	}{
		{"disk", `{"quotaHistoryMaxSamples":31,"maxConcurrentGenerations":7,"clientVersion":"1.20.0"}`, false, 31, 7, "1.20.0"},
		{"environment", `{"quotaHistoryMaxSamples":31,"maxConcurrentGenerations":7,"clientVersion":"1.20.0"}`, true, 51, 3, "1.21.0"},
		{"environment replaces invalid disk", `{"quotaHistoryMaxSamples":0,"maxConcurrentGenerations":-1,"clientVersion":""}`, true, 51, 3, "1.21.0"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			home := isolatedLoadHome(t)
			writeLoadConfig(t, home, test.contents)
			if test.environment {
				t.Setenv("ANTIGRAVITY_QUOTA_HISTORY_MAX_SAMPLES", "51")
				t.Setenv("ANTIGRAVITY_MAX_CONCURRENT_GENERATIONS", "3")
				t.Setenv("ANTIGRAVITY_CLIENT_VERSION", " 1.21.0 ")
			}
			cfg, _, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.QuotaHistoryMaxSamples != test.maxSamples || cfg.MaxConcurrentGenerations != test.maxGenerations || cfg.ClientVersion != test.version {
				t.Fatal("resource override precedence changed")
			}
		})
	}
}

func TestLoadRejectsInvalidResourceSettings(t *testing.T) {
	for _, field := range []struct{ key, env string }{
		{"quotaHistoryMaxSamples", "ANTIGRAVITY_QUOTA_HISTORY_MAX_SAMPLES"},
		{"maxConcurrentGenerations", "ANTIGRAVITY_MAX_CONCURRENT_GENERATIONS"},
	} {
		for _, value := range []string{"0", "-1", "", "invalid", "999999999999999999999999999"} {
			t.Run(field.key+"/environment/"+value, func(t *testing.T) {
				isolatedLoadHome(t)
				t.Setenv(field.env, value)
				if _, _, err := Load(); err == nil {
					t.Fatal("Load accepted invalid environment resource limit")
				}
			})
		}
		for _, value := range []string{"0", "-1"} {
			t.Run(field.key+"/disk/"+value, func(t *testing.T) {
				home := isolatedLoadHome(t)
				writeLoadConfig(t, home, `{"`+field.key+`":`+value+`}`)
				if _, _, err := Load(); err == nil {
					t.Fatal("Load accepted invalid saved resource limit")
				}
			})
		}
	}
	for _, useEnv := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty version env=%t", useEnv), func(t *testing.T) {
			home := isolatedLoadHome(t)
			if useEnv {
				t.Setenv("ANTIGRAVITY_CLIENT_VERSION", " ")
			} else {
				writeLoadConfig(t, home, `{"clientVersion":" "}`)
			}
			if _, _, err := Load(); err == nil {
				t.Fatal("Load accepted an empty client version")
			}
		})
	}
}

func TestSaveConcurrentWritersKeepCompletePrivateConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, Config{ProjectID: "initial", RefreshToken: "initial"}); err != nil {
		t.Fatal(err)
	}
	const writers = 24
	results := make(chan error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			value := fmt.Sprintf("writer-%d", i)
			results <- Save(path, Config{ProjectID: value, RefreshToken: value})
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
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal("concurrent save left incomplete JSON", err)
	}
	if cfg.ProjectID != cfg.RefreshToken || !strings.HasPrefix(cfg.ProjectID, "writer-") {
		t.Fatal("concurrent save mixed different writers' settings")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("saved configuration is not private")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Fatal("concurrent save leaked temporary files", err)
	}
}

func TestSaveFailureCleansTemporaryFile(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "config.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "keep"), []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, Config{RefreshToken: "new-token"}); err == nil {
		t.Fatal("Save replaced a nonempty directory")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Fatal("failed replacement leaked temporary config", err)
	}
	data, err := os.ReadFile(filepath.Join(path, "keep"))
	if err != nil || !bytes.Equal(data, []byte("unchanged")) {
		t.Fatal("failed save changed existing destination", err)
	}
}
