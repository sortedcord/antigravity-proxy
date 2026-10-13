package config

import (
	"fmt"
	"os"
	"strconv"
)

// EnvironmentVariables returns a fresh list for CLI documentation and isolated
// test environments. Runtime values are never included in this metadata.
func EnvironmentVariables() []string {
	return []string{
		"HOST", "PORT", "API_KEY", "ADMIN_API_KEY",
		"ANTIGRAVITY_ACCESS_TOKEN", "ANTIGRAVITY_REFRESH_TOKEN",
		"ANTIGRAVITY_OAUTH_CLIENT_ID", "ANTIGRAVITY_OAUTH_CLIENT_SECRET",
		"ANTIGRAVITY_PROJECT_ID", "ANTIGRAVITY_DAILY_ENDPOINT", "ANTIGRAVITY_PROD_ENDPOINT",
		"ANTIGRAVITY_CLIENT_VERSION", "OAUTH_CALLBACK_PORT",
		"ANTIGRAVITY_MAX_CONCURRENT_GENERATIONS", "ANTIGRAVITY_QUOTA_POLL_INTERVAL_SECONDS",
		"ANTIGRAVITY_QUOTA_HISTORY_PATH", "ANTIGRAVITY_QUOTA_HISTORY_MAX_SAMPLES",
	}
}

// TokenEnvironmentOverrides includes explicitly empty values: they still
// override saved identity and prevent management login from activating disk state.
func TokenEnvironmentOverrides() bool {
	_, access := os.LookupEnv("ANTIGRAVITY_ACCESS_TOKEN")
	_, refresh := os.LookupEnv("ANTIGRAVITY_REFRESH_TOKEN")
	return access || refresh
}

// OAuthCallbackPort returns zero when the preferred loopback port is unset.
func OAuthCallbackPort() (int, error) {
	value := os.Getenv("OAUTH_CALLBACK_PORT")
	if value == "" {
		return 0, nil
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("OAUTH_CALLBACK_PORT must be an integer from 1 to 65535")
	}
	return port, nil
}
