// Package config loads, validates, and persists proxy configuration.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultQuotaPollIntervalSeconds is the default interval between quota observations.
const DefaultQuotaPollIntervalSeconds = 300

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
	}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return Config{}, "", fmt.Errorf("read config %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return Config{}, "", fmt.Errorf("read config %s: %w", path, err)
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

// Save writes cfg through a same-directory temporary file and renames it over
// path. Newly created directories and files use owner-only permissions.
func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}
