// Package config loads, validates, and persists proxy configuration.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultQuotaPollIntervalSeconds is the default interval between quota observations.
const DefaultQuotaPollIntervalSeconds = 300

// DefaultQuotaHistoryMaxSamples bounds retained quota observations.
const DefaultQuotaHistoryMaxSamples = 10000

// DefaultClientVersion identifies the Antigravity client on all upstream requests.
const DefaultClientVersion = "1.15.8"

// DefaultMaxConcurrentGenerations bounds generation requests held in memory.
const DefaultMaxConcurrentGenerations = 2

// Config holds listener settings, client authentication, Google credentials,
// Antigravity project and endpoint settings, and quota polling settings.
type Config struct {
	Port int    `json:"port"`
	Host string `json:"host"`
	// APIKey authenticates proxy clients, not requests to Google.
	APIKey string `json:"apiKey,omitempty"`
	// AccessToken takes precedence over RefreshToken and is not refreshed.
	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
	// OAuth client credentials are saved with the refresh token that uses them.
	OAuthClientID     string `json:"oauthClientId,omitempty"`
	OAuthClientSecret string `json:"oauthClientSecret,omitempty"`
	ProjectID         string `json:"projectId,omitempty"`
	DailyEndpoint     string `json:"dailyEndpoint,omitempty"`
	ProdEndpoint      string `json:"prodEndpoint,omitempty"`
	// QuotaPollIntervalSeconds is the positive interval between quota polls in seconds.
	QuotaPollIntervalSeconds int `json:"quotaPollIntervalSeconds,omitempty"`
	// QuotaHistoryPath is the JSONL file used to persist quota observations.
	QuotaHistoryPath string `json:"quotaHistoryPath,omitempty"`
	// QuotaHistoryMaxSamples is the positive maximum number of retained observations.
	QuotaHistoryMaxSamples int `json:"quotaHistoryMaxSamples,omitempty"`
	// ClientVersion is shared by generation and quota upstream transports.
	ClientVersion string `json:"clientVersion,omitempty"`
	// MaxConcurrentGenerations bounds simultaneously processed generation requests.
	MaxConcurrentGenerations int `json:"maxConcurrentGenerations,omitempty"`
}

// Load reads ~/.config/antigravity-proxy/config.json, applies defaults and
// environment overrides, and validates listener, upstream, and quota settings.
// It returns the configuration and its persistence path; a missing file is allowed.
func Load() (Config, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, "", err
	}
	path := filepath.Join(home, ".config", "antigravity-proxy", "config.json")
	cfg := Config{
		Port:                     8080,
		Host:                     "127.0.0.1",
		QuotaPollIntervalSeconds: DefaultQuotaPollIntervalSeconds,
		QuotaHistoryPath:         filepath.Join(home, ".local", "share", "antigravity-proxy", "usage.jsonl"),
		QuotaHistoryMaxSamples:   DefaultQuotaHistoryMaxSamples,
		ClientVersion:            DefaultClientVersion,
		MaxConcurrentGenerations: DefaultMaxConcurrentGenerations,
	}
	if _, err := readConfig(path, &cfg); err != nil {
		return Config{}, "", err
	}
	if cfg.Port == 0 {
		cfg.Port = 8080
	}
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	envString("HOST", &cfg.Host)
	cfg.Host = strings.TrimSpace(cfg.Host)
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if value := os.Getenv("PORT"); value != "" {
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return Config{}, "", fmt.Errorf("PORT must be an integer from 1 to 65535")
		}
		cfg.Port = port
	}
	envString("API_KEY", &cfg.APIKey)
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	if err := validateListenAddress(cfg.Host, cfg.APIKey); err != nil {
		return Config{}, "", err
	}
	envString("ANTIGRAVITY_ACCESS_TOKEN", &cfg.AccessToken)
	envString("ANTIGRAVITY_REFRESH_TOKEN", &cfg.RefreshToken)
	// Load OAuth sources without requiring them for explicit access-token use.
	// Login and Refresh validate the selected pair when OAuth is actually needed.
	if id, secret, provided := oauthEnvironmentPair(); provided {
		cfg.OAuthClientID, cfg.OAuthClientSecret = id, secret
	} else {
		cfg.OAuthClientID = strings.TrimSpace(cfg.OAuthClientID)
		cfg.OAuthClientSecret = strings.TrimSpace(cfg.OAuthClientSecret)
	}
	envString("ANTIGRAVITY_PROJECT_ID", &cfg.ProjectID)
	envString("ANTIGRAVITY_DAILY_ENDPOINT", &cfg.DailyEndpoint)
	envString("ANTIGRAVITY_PROD_ENDPOINT", &cfg.ProdEndpoint)
	if cfg.DailyEndpoint == "" {
		cfg.DailyEndpoint = "https://daily-cloudcode-pa.googleapis.com"
	}
	if cfg.ProdEndpoint == "" {
		cfg.ProdEndpoint = "https://cloudcode-pa.googleapis.com"
	}
	cfg.DailyEndpoint = strings.TrimRight(cfg.DailyEndpoint, "/")
	cfg.ProdEndpoint = strings.TrimRight(cfg.ProdEndpoint, "/")
	if err := validateCloudEndpoint("ANTIGRAVITY_DAILY_ENDPOINT", cfg.DailyEndpoint); err != nil {
		return Config{}, "", err
	}
	if err := validateCloudEndpoint("ANTIGRAVITY_PROD_ENDPOINT", cfg.ProdEndpoint); err != nil {
		return Config{}, "", err
	}
	if value, ok := os.LookupEnv("ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS"); ok {
		seconds, err := strconv.Atoi(value)
		if err != nil {
			return Config{}, "", fmt.Errorf("ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS must be a positive integer number of seconds")
		}
		cfg.QuotaPollIntervalSeconds = seconds
	}
	const maxQuotaPollIntervalSeconds = int64((1<<63 - 1) / time.Second)
	if cfg.QuotaPollIntervalSeconds <= 0 || int64(cfg.QuotaPollIntervalSeconds) > maxQuotaPollIntervalSeconds {
		return Config{}, "", fmt.Errorf("quotaPollIntervalSeconds must be a positive integer no greater than %d seconds", maxQuotaPollIntervalSeconds)
	}
	envString("ANTIGRAVITY_QUOTA_HISTORY_PATH", &cfg.QuotaHistoryPath)
	if strings.TrimSpace(cfg.QuotaHistoryPath) == "" {
		return Config{}, "", fmt.Errorf("quotaHistoryPath must not be empty")
	}
	for _, setting := range []struct {
		envName  string
		jsonName string
		value    *int
	}{
		{"ANTIGRAVITY_QUOTA_HISTORY_MAX_SAMPLES", "quotaHistoryMaxSamples", &cfg.QuotaHistoryMaxSamples},
		{"ANTIGRAVITY_MAX_CONCURRENT_GENERATIONS", "maxConcurrentGenerations", &cfg.MaxConcurrentGenerations},
	} {
		if value, ok := os.LookupEnv(setting.envName); ok {
			number, err := strconv.Atoi(value)
			if err != nil {
				return Config{}, "", fmt.Errorf("%s must be a positive integer", setting.envName)
			}
			*setting.value = number
		}
		if *setting.value <= 0 {
			return Config{}, "", fmt.Errorf("%s must be a positive integer", setting.jsonName)
		}
	}
	envString("ANTIGRAVITY_CLIENT_VERSION", &cfg.ClientVersion)
	cfg.ClientVersion = strings.TrimSpace(cfg.ClientVersion)
	if cfg.ClientVersion == "" {
		return Config{}, "", fmt.Errorf("clientVersion must not be empty")
	}
	return cfg, path, nil
}

// OAuthCredentials resolves a whole environment pair before the saved pair.
// An absent pair is allowed for login discovery; a partial pair is never mixed.
func (cfg Config) OAuthCredentials() (string, string, error) {
	id, secret, provided := oauthEnvironmentPair()
	if provided {
		if id == "" || secret == "" {
			return "", "", fmt.Errorf("ANTIGRAVITY_OAUTH_CLIENT_ID and ANTIGRAVITY_OAUTH_CLIENT_SECRET must be provided together")
		}
		return id, secret, nil
	}
	id, secret = strings.TrimSpace(cfg.OAuthClientID), strings.TrimSpace(cfg.OAuthClientSecret)
	if (id == "") != (secret == "") {
		return "", "", fmt.Errorf("oauthClientId and oauthClientSecret must be provided together; run antigravity-proxy login or configure a complete environment pair")
	}
	return id, secret, nil
}

func oauthEnvironmentPair() (id, secret string, provided bool) {
	id, idSet := os.LookupEnv("ANTIGRAVITY_OAUTH_CLIENT_ID")
	secret, secretSet := os.LookupEnv("ANTIGRAVITY_OAUTH_CLIENT_SECRET")
	id, secret = strings.TrimSpace(id), strings.TrimSpace(secret)
	return id, secret, idSet != secretSet || id != "" || secret != ""
}

func envString(name string, dst *string) {
	if value, ok := os.LookupEnv(name); ok {
		*dst = value
	}
}

func validateListenAddress(host, apiKey string) error {
	ip := net.ParseIP(host)
	if strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback()) {
		return nil
	}
	if strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("API_KEY is required when HOST is not localhost or a loopback address")
	}
	return nil
}

func validateCloudEndpoint(name, endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s must be an HTTPS base URL", name)
	}
	if parsed.Scheme == "https" {
		return nil
	}
	ip := net.ParseIP(parsed.Hostname())
	isLoopback := strings.EqualFold(parsed.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())
	if parsed.Scheme == "http" && isLoopback {
		return nil
	}
	return fmt.Errorf("%s must use HTTPS; plain HTTP is allowed only for localhost/loopback testing", name)
}

// readConfig reads only disk settings, without defaults or environment overrides.
// Missing files leave cfg unchanged. Duplicate top-level fields are rejected even
// when their values agree, using the case-insensitive matching of encoding/json.
// Unix permission bits have no ACL meaning on Windows, which uses directory ACLs.
func readConfig(path string, cfg *Config) ([]byte, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat config %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("config %s must be a regular file", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("config %s must have owner-only permissions (chmod 600)", path)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("config %s must contain a JSON object", path)
	}
	if err := rejectDuplicateConfigFields(data); err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("config %s must contain exactly one JSON object", path)
	}
	return data, nil
}

// Check decoded keys before decoding into Config or a map: both would otherwise
// silently overwrite aliases, and a later map serialization can change the winner.
func rejectDuplicateConfigFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if _, err := decoder.Token(); err != nil {
		return err
	}
	var keys []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key := token.(string)
		for _, previous := range keys {
			if strings.EqualFold(key, previous) {
				return fmt.Errorf("duplicate case-insensitive top-level config fields")
			}
		}
		keys = append(keys, key)
		// Consume one value without retaining it, including any nested containers.
		depth := 0
		for {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			switch token {
			case json.Delim('{'), json.Delim('['):
				depth++
			case json.Delim('}'), json.Delim(']'):
				depth--
			}
			if depth == 0 {
				break
			}
		}
	}
	_, err := decoder.Token()
	return err
}

// UpdateCredentials rereads disk and changes only the OAuth pair and token fields.
// It never persists defaults, environment overrides, or a stale login snapshot.
func UpdateCredentials(path, accessToken, refreshToken, clientID, clientSecret string) error {
	var disk Config
	data, err := readConfig(path, &disk)
	if err != nil {
		return err
	}
	fields := make(map[string]json.RawMessage)
	if len(data) != 0 {
		if err := json.Unmarshal(data, &fields); err != nil {
			return fmt.Errorf("read saved credentials: %w", err)
		}
	}
	// encoding/json matches Config keys case-insensitively. Remove every alias
	// before inserting canonical keys so an old static token cannot survive login.
	for key := range fields {
		if strings.EqualFold(key, "accessToken") || strings.EqualFold(key, "refreshToken") ||
			strings.EqualFold(key, "oauthClientId") || strings.EqualFold(key, "oauthClientSecret") {
			delete(fields, key)
		}
	}
	for key, value := range map[string]string{
		"accessToken":       accessToken,
		"refreshToken":      refreshToken,
		"oauthClientId":     clientID,
		"oauthClientSecret": clientSecret,
	} {
		if value == "" {
			continue
		}
		fields[key], _ = json.Marshal(value)
	}
	data, err = json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	return saveConfigBytes(path, data)
}

// Save durably replaces cfg using a unique same-directory private temporary file.
// Concurrent writers never share temporary files; the last rename wins.
func Save(path string, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return saveConfigBytes(path, data)
}

func saveConfigBytes(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync config: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	// Go cannot open directories for Sync on Windows.
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open config directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync config directory: %w", err)
	}
	return nil
}
