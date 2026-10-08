package config

import (
	"bytes"
	"encoding/json"
	"errors"
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
			path := writeLoadConfig(t, home, contents)
			if _, _, err := Load(); err == nil {
				t.Fatal("Load accepted invalid or unknown configuration")
			}
			if err := UpdateCredentials(path, Credentials{RefreshToken: "new-token", AccountID: "new-account"}); err == nil {
				t.Fatal("credential update accepted invalid or unknown configuration")
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != contents {
				t.Fatal("rejected configuration update changed disk", err)
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
		{"accountId", `"first-account"`, `"second-account"`},
		{"accountEmail", `"first@example.com"`, `"second@example.com"`},
		{"accountName", `"First Name"`, `"Second Name"`},
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
						return UpdateCredentials(path, Credentials{AccessToken: "new-access", RefreshToken: "new-refresh", OAuthClientID: "new-id", OAuthClientSecret: "new-secret"})
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
	if err := UpdateCredentials(path, Credentials{AccessToken: "new-access", RefreshToken: "new-refresh", OAuthClientID: "new-id", OAuthClientSecret: "new-secret"}); err != nil {
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

func TestUpdateCredentialsCanonicalizesAccountIdentityAsSet(t *testing.T) {
	home := isolatedLoadHome(t)
	path := writeLoadConfig(t, home, `{"HOST":"localhost","ProjectID":"keep","ACCESS\u0054OKEN":"old-access","REFRESHTOKEN":"old-refresh","OAuthClientID":"old-client","OAUTHCLIENTSECRET":"old-secret","ACCOUNT\u0049D":"old-account","AccountEmail":"old@example.com","ACCOUNTNAME":"Old Name"}`)
	initial, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if initial.AccountID != "old-account" || initial.AccountEmail != "old@example.com" || initial.AccountName != "Old Name" {
		t.Fatal("unique case and escaped identity aliases did not load")
	}
	creds := Credentials{
		RefreshToken: "new-refresh", OAuthClientID: "new-client", OAuthClientSecret: "new-secret",
		AccountID: "new-account", AccountEmail: "new@example.com", AccountName: "New Name",
	}
	if err := UpdateCredentials(path, creds); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"HOST": "localhost", "ProjectID": "keep", "refreshToken": "new-refresh",
		"oauthClientId": "new-client", "oauthClientSecret": "new-secret",
		"accountId": "new-account", "accountEmail": "new@example.com", "accountName": "New Name",
	}
	if len(fields) != len(want) {
		t.Fatal("credential update retained aliases or inserted unrelated defaults")
	}
	for key, value := range want {
		var got string
		if err := json.Unmarshal(fields[key], &got); err != nil || got != value {
			t.Fatalf("field %s was not preserved or canonicalized", key)
		}
	}
	cfg, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccessToken != "" || cfg.RefreshToken != creds.RefreshToken || cfg.AccountID != creds.AccountID || cfg.AccountEmail != creds.AccountEmail || cfg.AccountName != creds.AccountName {
		t.Fatal("updated credentials and identity did not load together")
	}
	if err := UpdateCredentials(path, Credentials{AccessToken: "static-access"}); err != nil {
		t.Fatal(err)
	}
	cfg, _, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccessToken != "static-access" || cfg.RefreshToken != "" || cfg.OAuthClientID != "" || cfg.OAuthClientSecret != "" || cfg.AccountID != "" || cfg.AccountEmail != "" || cfg.AccountName != "" {
		t.Fatal("token-only replacement retained old credentials or account metadata")
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Unmarshal into a fresh map because it otherwise retains removed keys.
	fields = nil
	if err := json.Unmarshal(data, &fields); err != nil || len(fields) != 3 {
		t.Fatal("token-only replacement did not remove identity and OAuth keys")
	}
}

func TestLoadTokenEnvironmentOverrideClearsSavedIdentity(t *testing.T) {
	for _, test := range []struct {
		name, env, value string
	}{
		{"saved", "", ""},
		{"access", "ANTIGRAVITY_ACCESS_TOKEN", "environment-access"},
		{"same access", "ANTIGRAVITY_ACCESS_TOKEN", "saved-access"},
		{"empty access", "ANTIGRAVITY_ACCESS_TOKEN", ""},
		{"refresh", "ANTIGRAVITY_REFRESH_TOKEN", "environment-refresh"},
		{"same refresh", "ANTIGRAVITY_REFRESH_TOKEN", "saved-refresh"},
		{"empty refresh", "ANTIGRAVITY_REFRESH_TOKEN", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := isolatedLoadHome(t)
			contents := `{"accessToken":"saved-access","refreshToken":"saved-refresh","accountId":"saved-id","accountEmail":"saved@example.com","accountName":"Saved Name"}`
			path := writeLoadConfig(t, home, contents)
			if test.env != "" {
				t.Setenv(test.env, test.value)
			}
			cfg, _, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if test.env == "" {
				if cfg.AccountID != "saved-id" || cfg.AccountEmail != "saved@example.com" || cfg.AccountName != "Saved Name" {
					t.Fatal("saved identity was lost without a token override")
				}
			} else if cfg.AccountID != "" || cfg.AccountEmail != "" || cfg.AccountName != "" {
				t.Fatal("environment credentials inherited saved account attribution")
			}
			wantAccess, wantRefresh := "saved-access", "saved-refresh"
			if test.env == "ANTIGRAVITY_ACCESS_TOKEN" {
				wantAccess = test.value
			} else if test.env == "ANTIGRAVITY_REFRESH_TOKEN" {
				wantRefresh = test.value
			}
			if cfg.AccessToken != wantAccess || cfg.RefreshToken != wantRefresh {
				t.Fatal("token overrides changed unrelated credentials")
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != contents {
				t.Fatal("Load persisted environment overrides or cleared disk identity", err)
			}
		})
	}
}

func TestUpdateCredentialsPreservesDiskWithoutEnvironmentOrDefaults(t *testing.T) {
	home := isolatedLoadHome(t)
	path := writeLoadConfig(t, home, `{"projectId":"first-project","accountId":"old-account","accountEmail":"old@example.com","accountName":"Old Name"}`)
	t.Setenv("ANTIGRAVITY_ACCESS_TOKEN", "environment-access")
	t.Setenv("ANTIGRAVITY_REFRESH_TOKEN", "environment-refresh")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", "environment-client")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", "environment-secret")
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "environment-project")
	if _, _, err := Load(); err != nil {
		t.Fatal(err)
	}
	// Simulate a disk edit after login began; credential persistence must reread it.
	if err := os.WriteFile(path, []byte(`{"projectId":"latest-project","accountId":"old-account"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := UpdateCredentials(path, Credentials{RefreshToken: "new-refresh", OAuthClientID: "new-client", OAuthClientSecret: "new-secret", AccountID: "new-account", AccountEmail: "new@example.com", AccountName: "New Name"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 7 || string(fields["projectId"]) != `"latest-project"` || string(fields["accountId"]) != `"new-account"` {
		t.Fatal("credential persistence wrote stale settings, defaults, or failed to replace identity")
	}
	for _, marker := range []string{"environment-access", "environment-refresh", "environment-client", "environment-secret", "environment-project", "old-account"} {
		if bytes.Contains(data, []byte(marker)) {
			t.Fatal("credential persistence leaked environment values or retained old identity")
		}
	}
}

type failingConfigTemporaryFile struct {
	configTemporaryFile
	stage string
	err   error
}

func (file *failingConfigTemporaryFile) Write(data []byte) (int, error) {
	if file.stage == "write" {
		return 0, file.err
	}
	return file.configTemporaryFile.Write(data)
}

func (file *failingConfigTemporaryFile) Sync() error {
	if file.stage == "file sync" {
		return file.err
	}
	return file.configTemporaryFile.Sync()
}

func (file *failingConfigTemporaryFile) Close() error {
	err := file.configTemporaryFile.Close()
	if file.stage == "file close" {
		return file.err
	}
	return err
}

type recordingConfigDirectory struct {
	syncErr   error
	syncCalls int
	closed    bool
}

func (directory *recordingConfigDirectory) Sync() error {
	directory.syncCalls++
	return directory.syncErr
}

func (directory *recordingConfigDirectory) Close() error {
	directory.closed = true
	return nil
}

func TestConfigSavePrecommitFailuresPreserveDestination(t *testing.T) {
	for _, stage := range []string{"mkdir", "directory open", "temporary create", "write", "file sync", "file close", "rename"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.json")
			old := []byte(`{"refreshToken":"old-token","accountId":"old-account"}`)
			if err := os.WriteFile(path, old, 0600); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("injected precommit filesystem failure")
			fs := localConfigFilesystem()
			directory := &recordingConfigDirectory{}
			opened := false
			fs.openDirectory = func(string) (configDirectory, error) {
				if stage == "directory open" {
					return nil, failure
				}
				opened = true
				return directory, nil
			}
			createTemp := fs.createTemp
			fs.createTemp = func(dir, pattern string) (configTemporaryFile, error) {
				if !opened {
					t.Fatal("temporary creation began before opening the directory")
				}
				if stage == "temporary create" {
					return nil, failure
				}
				file, err := createTemp(dir, pattern)
				if err != nil {
					return nil, err
				}
				return &failingConfigTemporaryFile{configTemporaryFile: file, stage: stage, err: failure}, nil
			}
			if stage == "mkdir" {
				fs.mkdirAll = func(string, os.FileMode) error { return failure }
			}
			fs.rename = func(string, string) error {
				if stage != "rename" {
					t.Fatal("precommit failure still attempted replacement")
				}
				return failure
			}
			err := saveConfigBytesWithFS(path, []byte(`{"refreshToken":"new-token"}`), fs)
			var committed *CommitError
			if !errors.Is(err, failure) || errors.As(err, &committed) {
				t.Fatal("precommit failure did not return an ordinary wrapped error", err)
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(data, old) {
				t.Fatal("precommit failure changed saved credentials", readErr)
			}
			if directory.syncCalls != 0 || (opened && !directory.closed) {
				t.Fatal("precommit failure synced or leaked the directory handle")
			}
			entries, readErr := os.ReadDir(filepath.Dir(path))
			if readErr != nil || len(entries) != 1 || entries[0].Name() != "config.json" {
				t.Fatal("precommit failure leaked temporary files", readErr)
			}
		})
	}
}

func TestConfigSavePostcommitDirectorySyncFailureReportsCommitted(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"refreshToken":"old-token"}`), 0600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected secret-marker /private/config/path sync failure")
	directory := &recordingConfigDirectory{syncErr: failure}
	fs := localConfigFilesystem()
	opened := false
	fs.openDirectory = func(string) (configDirectory, error) {
		opened = true
		return directory, nil
	}
	rename := fs.rename
	fs.rename = func(oldPath, newPath string) error {
		if !opened || directory.closed || directory.syncCalls != 0 {
			t.Fatal("directory was not opened and retained before replacement")
		}
		return rename(oldPath, newPath)
	}
	want := []byte("{\"refreshToken\":\"new-token\",\"accountId\":\"new-account\"}\n")
	err := saveConfigBytesWithFS(path, bytes.TrimSuffix(want, []byte("\n")), fs)
	var committed *CommitError
	if !errors.As(err, &committed) || !errors.Is(err, failure) || committed.Err != failure {
		t.Fatal("postcommit failure did not expose the committed outcome and cause", err)
	}
	if strings.Contains(err.Error(), "secret-marker") || strings.Contains(err.Error(), "/private/config/path") || !strings.Contains(err.Error(), "replaced") || !strings.Contains(err.Error(), "retry saving") {
		t.Fatal("committed outcome was not sanitized and actionable")
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(data, want) {
		t.Fatal("postcommit failure did not preserve the new saved credentials", readErr)
	}
	if directory.syncCalls != 1 || !directory.closed {
		t.Fatal("postcommit failure did not sync and close the retained directory")
	}
	info, statErr := os.Stat(path)
	if statErr != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatal("postcommit configuration was not private", statErr)
	}
	entries, readErr := os.ReadDir(filepath.Dir(path))
	if readErr != nil || len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Fatal("postcommit failure leaked temporary files", readErr)
	}
}
