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
)

type Config struct {
	Port          int    `json:"port"`
	Host          string `json:"host"`
	APIKey        string `json:"apiKey,omitempty"`
	AccessToken   string `json:"accessToken,omitempty"`
	RefreshToken  string `json:"refreshToken,omitempty"`
	ProjectID     string `json:"projectId,omitempty"`
	DailyEndpoint string `json:"dailyEndpoint,omitempty"`
	ProdEndpoint  string `json:"prodEndpoint,omitempty"`
}

func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "antigravity-proxy", "config.json"), nil
}

func Load() (Config, string, error) {
	path, err := configPath()
	if err != nil {
		return Config{}, "", err
	}
	cfg := Config{Port: 8080, Host: "127.0.0.1"}
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
	return cfg, path, nil
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
