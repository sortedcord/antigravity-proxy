package oauth

import (
	"context"

	"antigravity-proxy/internal/config"
)

const (
	consumerAppTag   = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	consumerTokenKey = "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"
)

// resolveLoginCredentials resolves the OAuth client ID and secret to use for
// Google authorization. If ANTIGRAVITY_OAUTH_CLIENT_ID and
// ANTIGRAVITY_OAUTH_CLIENT_SECRET are set in the environment, they override the
// official consumer client pair.
func resolveLoginCredentials(ctx context.Context) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	id, secret, err := (config.Config{}).OAuthCredentials()
	if err != nil || id != "" {
		return id, secret, err
	}
	return consumerAppTag, consumerTokenKey, nil
}
