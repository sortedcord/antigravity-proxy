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

	"antigravity-proxy/internal/atomicfile"
)

const (
	// DefaultPort is the default local HTTP port.
	DefaultPort = 8080
	// DefaultHost keeps the listener private unless explicitly configured.
	DefaultHost = "127.0.0.1"
	// DefaultDailyEndpoint and DefaultProdEndpoint identify Cloud Code origins.
	DefaultDailyEndpoint = "https://daily-cloudcode-pa.googleapis.com"
	DefaultProdEndpoint  = "https://cloudcode-pa.googleapis.com"
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
	// Account identity belongs to the saved token set, never an environment override.
	AccountID     string `json:"accountId,omitempty"`
	AccountEmail  string `json:"accountEmail,omitempty"`
	AccountName   string `json:"accountName,omitempty"`
	ProjectID     string `json:"projectId,omitempty"`
	DailyEndpoint string `json:"dailyEndpoint,omitempty"`
	ProdEndpoint  string `json:"prodEndpoint,omitempty"`
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

// Credentials is a complete token, OAuth client, and account identity replacement.
type Credentials struct {
	AccessToken       string
	RefreshToken      string
	OAuthClientID     string
	OAuthClientSecret string
	AccountID         string
	AccountEmail      string
	AccountName       string
}

// CommitError means the configuration was replaced, but directory durability was
// not confirmed. Callers must use the committed credentials rather than report
// that the old configuration remains active.
type CommitError struct {
	Err error
}

func (err *CommitError) Error() string {
	return "configuration was replaced, but its durability could not be confirmed; retry saving to confirm durability"
}

func (err *CommitError) Unwrap() error {
	return err.Err
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
	cfg := defaults(home)
	if _, err := readConfig(path, &cfg); err != nil {
		return Config{}, "", err
	}
	if err := cfg.applyEnvironment(); err != nil {
		return Config{}, "", err
	}
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return Config{}, "", err
	}
	return cfg, path, nil
}

func defaults(home string) Config {
	return Config{
		Port:                     DefaultPort,
		Host:                     DefaultHost,
		QuotaPollIntervalSeconds: DefaultQuotaPollIntervalSeconds,
		QuotaHistoryPath:         filepath.Join(home, ".local", "share", "antigravity-proxy", "usage.jsonl"),
		QuotaHistoryMaxSamples:   DefaultQuotaHistoryMaxSamples,
		ClientVersion:            DefaultClientVersion,
		MaxConcurrentGenerations: DefaultMaxConcurrentGenerations,
	}
}

func (cfg *Config) applyEnvironment() error {
	envString("HOST", &cfg.Host)
	if value := os.Getenv("PORT"); value != "" {
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("PORT must be an integer from 1 to 65535")
		}
		cfg.Port = port
	}
	envString("API_KEY", &cfg.APIKey)
	accessToken, accessSet := os.LookupEnv("ANTIGRAVITY_ACCESS_TOKEN")
	refreshToken, refreshSet := os.LookupEnv("ANTIGRAVITY_REFRESH_TOKEN")
	if accessSet {
		cfg.AccessToken = accessToken
	}
	if refreshSet {
		cfg.RefreshToken = refreshToken
	}
	if accessSet || refreshSet {
		cfg.AccountID, cfg.AccountEmail, cfg.AccountName = "", "", ""
	}
	// Load OAuth sources without requiring them for explicit access-token use.
	// Login and Refresh validate the selected pair when OAuth is actually needed.
	if id, secret, provided := oauthEnvironmentPair(); provided {
		cfg.OAuthClientID, cfg.OAuthClientSecret = id, secret
	}
	envString("ANTIGRAVITY_PROJECT_ID", &cfg.ProjectID)
	envString("ANTIGRAVITY_DAILY_ENDPOINT", &cfg.DailyEndpoint)
	envString("ANTIGRAVITY_PROD_ENDPOINT", &cfg.ProdEndpoint)
	if value, ok := os.LookupEnv("ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS"); ok {
		seconds, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS must be a positive integer number of seconds")
		}
		cfg.QuotaPollIntervalSeconds = seconds
	}
	envString("ANTIGRAVITY_QUOTA_HISTORY_PATH", &cfg.QuotaHistoryPath)
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
				return fmt.Errorf("%s must be a positive integer", setting.envName)
			}
			*setting.value = number
		}
	}
	envString("ANTIGRAVITY_CLIENT_VERSION", &cfg.ClientVersion)
	return nil
}

func (cfg *Config) normalize() {
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	cfg.Host = strings.TrimSpace(cfg.Host)
	if cfg.Host == "" {
		cfg.Host = DefaultHost
	}
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.OAuthClientID = strings.TrimSpace(cfg.OAuthClientID)
	cfg.OAuthClientSecret = strings.TrimSpace(cfg.OAuthClientSecret)
	if cfg.DailyEndpoint == "" {
		cfg.DailyEndpoint = DefaultDailyEndpoint
	}
	if cfg.ProdEndpoint == "" {
		cfg.ProdEndpoint = DefaultProdEndpoint
	}
	cfg.DailyEndpoint = strings.TrimRight(cfg.DailyEndpoint, "/")
	cfg.ProdEndpoint = strings.TrimRight(cfg.ProdEndpoint, "/")
	cfg.ClientVersion = strings.TrimSpace(cfg.ClientVersion)
}

// Validate checks effective settings without reading the environment, applying
// defaults, or validating OAuth pairs that explicit access-token use may bypass.
func (cfg Config) Validate() error {
	if cfg.Port < 1 || cfg.Port > 65535 {
		return fmt.Errorf("port must be an integer from 1 to 65535")
	}
	if err := validateListenAddress(cfg.Host, cfg.APIKey); err != nil {
		return err
	}
	if err := validateCloudEndpoint("ANTIGRAVITY_DAILY_ENDPOINT", cfg.DailyEndpoint); err != nil {
		return err
	}
	if err := validateCloudEndpoint("ANTIGRAVITY_PROD_ENDPOINT", cfg.ProdEndpoint); err != nil {
		return err
	}
	if err := ValidatePollInterval(cfg.QuotaPollIntervalSeconds); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.QuotaHistoryPath) == "" {
		return fmt.Errorf("quotaHistoryPath must not be empty")
	}
	if cfg.QuotaHistoryMaxSamples <= 0 {
		return fmt.Errorf("quotaHistoryMaxSamples must be a positive integer")
	}
	if cfg.MaxConcurrentGenerations <= 0 {
		return fmt.Errorf("maxConcurrentGenerations must be a positive integer")
	}
	if cfg.ClientVersion == "" {
		return fmt.Errorf("clientVersion must not be empty")
	}
	return nil
}

// ValidatePollInterval rejects seconds that would overflow a time.Duration.
func ValidatePollInterval(seconds int) error {
	const maxSeconds = int64((1<<63 - 1) / time.Second)
	if seconds <= 0 || int64(seconds) > maxSeconds {
		return fmt.Errorf("quotaPollIntervalSeconds must be a positive integer no greater than %d seconds", maxSeconds)
	}
	return nil
}

// WithProxyDefaults fills the optional settings accepted by direct constructors.
// Load instead validates explicit zero/negative resource settings as errors.
func (cfg Config) WithProxyDefaults() Config {
	if cfg.ClientVersion == "" {
		cfg.ClientVersion = DefaultClientVersion
	}
	if cfg.MaxConcurrentGenerations <= 0 {
		cfg.MaxConcurrentGenerations = DefaultMaxConcurrentGenerations
	}
	return cfg
}

// OAuthCredentials resolves a whole environment pair before the saved pair.
// An absent pair is allowed for login discovery; a partial pair is never mixed.
func (cfg Config) OAuthCredentials() (string, string, error) {
	id, secret, provided := oauthEnvironmentPair()
	if provided {
		return validateEnvironmentOAuthPair(id, secret)
	}
	id, secret = strings.TrimSpace(cfg.OAuthClientID), strings.TrimSpace(cfg.OAuthClientSecret)
	if (id == "") != (secret == "") {
		return "", "", fmt.Errorf("oauthClientId and oauthClientSecret must be provided together; run antigravity-proxy login or configure a complete environment pair")
	}
	return id, secret, nil
}

// OAuthEnvironmentCredentials resolves a complete environment client pair.
// An absent pair returns empty strings; a partial pair never falls back to disk.
func OAuthEnvironmentCredentials() (string, string, error) {
	id, secret, provided := oauthEnvironmentPair()
	if !provided {
		return "", "", nil
	}
	return validateEnvironmentOAuthPair(id, secret)
}

func validateEnvironmentOAuthPair(id, secret string) (string, string, error) {
	if id == "" || secret == "" {
		return "", "", fmt.Errorf("ANTIGRAVITY_OAUTH_CLIENT_ID and ANTIGRAVITY_OAUTH_CLIENT_SECRET must be provided together")
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
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
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
		// Let encoding/json consume the complete value, including nested containers.
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}
	_, err := decoder.Token()
	return err
}

// UpdateCredentials rereads disk and replaces tokens, the OAuth pair, and account
// identity as a set. It never persists defaults, environment overrides, or a
// stale login snapshot. A CommitError means the replacement already committed.
func UpdateCredentials(path string, creds Credentials) error {
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
	for _, field := range []struct {
		key   string
		value string
	}{
		{key: "accessToken", value: creds.AccessToken},
		{key: "refreshToken", value: creds.RefreshToken},
		{key: "oauthClientId", value: creds.OAuthClientID},
		{key: "oauthClientSecret", value: creds.OAuthClientSecret},
		{key: "accountId", value: creds.AccountID},
		{key: "accountEmail", value: creds.AccountEmail},
		{key: "accountName", value: creds.AccountName},
	} {
		// encoding/json matches Config keys case-insensitively. Remove every
		// alias, even for empty replacements, so obsolete credentials cannot survive.
		for key := range fields {
			if strings.EqualFold(key, field.key) {
				delete(fields, key)
			}
		}
		if field.value != "" {
			fields[field.key], _ = json.Marshal(field.value)
		}
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

type configTemporaryFile interface {
	Name() string
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

type configDirectory interface {
	Sync() error
	Close() error
}

// configFilesystem is supplied per save so fault injection cannot affect other
// concurrent callers. A nil openDirectory means directory sync is unsupported.
type configFilesystem struct {
	mkdirAll      func(string, os.FileMode) error
	createTemp    func(string, string) (configTemporaryFile, error)
	openDirectory func(string) (configDirectory, error)
	rename        func(string, string) error
	remove        func(string) error
}

func localConfigFilesystem() configFilesystem {
	fs := configFilesystem{
		mkdirAll: os.MkdirAll,
		createTemp: func(dir, pattern string) (configTemporaryFile, error) {
			return os.CreateTemp(dir, pattern)
		},
		rename: atomicfile.Replace,
		remove: os.Remove,
	}
	// Go cannot open directories for Sync on Windows.
	if runtime.GOOS != "windows" {
		fs.openDirectory = func(path string) (configDirectory, error) {
			return os.Open(path)
		}
	}
	return fs
}

func saveConfigBytes(path string, data []byte) error {
	return saveConfigBytesWithFS(path, data, localConfigFilesystem())
}

func saveConfigBytesWithFS(path string, data []byte, fs configFilesystem) error {
	dir := filepath.Dir(path)
	if err := fs.mkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	// Open before rename: failure must leave the previous configuration intact.
	var directory configDirectory
	if fs.openDirectory != nil {
		var err error
		directory, err = fs.openDirectory(dir)
		if err != nil {
			return fmt.Errorf("open config directory for sync: %w", err)
		}
		defer directory.Close()
	}
	file, err := fs.createTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmp := file.Name()
	defer fs.remove(tmp)
	defer file.Close()
	record := append(data, '\n')
	n, err := file.Write(record)
	if err == nil && n != len(record) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync config: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	if err := fs.rename(tmp, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	if directory != nil {
		if err := directory.Sync(); err != nil {
			return &CommitError{Err: err}
		}
	}
	return nil
}
