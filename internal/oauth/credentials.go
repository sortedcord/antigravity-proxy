package oauth

import (
	"antigravity-proxy/internal/config"
)

const (
	consumerClientID     = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	consumerClientSecret = "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"
)

// resolveLoginCredentials resolves the OAuth client ID and secret to use for
// Google authorization. If ANTIGRAVITY_OAUTH_CLIENT_ID and
// ANTIGRAVITY_OAUTH_CLIENT_SECRET are set in the environment, they override the
// official consumer client pair.
func resolveLoginCredentials() (string, string, error) {
	id, secret, err := config.OAuthEnvironmentCredentials()
	if err != nil || id != "" {
		return id, secret, err
	}
	return consumerClientID, consumerClientSecret, nil
}
