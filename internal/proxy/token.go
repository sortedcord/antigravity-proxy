package proxy

import (
	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/oauth"
	"antigravity-proxy/internal/quota"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// tokenSource owns refresh serialization and token lifetime. cfg and client are
// immutable; mu protects cachedToken and expiresAt. Account promotion uses the
// snapshot/seed methods instead of sharing locks across runtime snapshots.
type tokenSource struct {
	cfg    *config.Config
	client *http.Client

	mu          sync.Mutex
	cachedToken string
	expiresAt   time.Time
}

func (s *tokenSource) seed(token string, expires time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cachedToken, s.expiresAt = token, expires
}

func (s *tokenSource) snapshot() (string, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cachedToken, s.expiresAt
}

// token uses an explicit access token unchanged. Polls never wait for an
// unrelated refresh owner: busy polls retry on a later scheduled attempt.
func (s *tokenSource) token(ctx context.Context, poll bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.cfg.AccessToken != "" {
		return s.cfg.AccessToken, nil
	}
	if s.cfg.RefreshToken == "" {
		return "", errors.New("no Google credential configured; use /admin/login-sessions or configure credentials")
	}
	if poll {
		if !s.mu.TryLock() {
			return "", quota.ErrAuthenticationBusy
		}
	} else {
		s.mu.Lock()
	}
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.cachedToken != "" && time.Until(s.expiresAt) > time.Minute {
		return s.cachedToken, nil
	}
	tokens, err := oauth.RefreshWithClient(ctx, *s.cfg, s.client)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		slog.Warn("upstream authentication failed", "operation", "oauth_refresh")
		return "", errors.New("Google OAuth refresh failed")
	}
	if tokens.AccessToken == "" {
		return "", errors.New("Google OAuth refresh response contained no access token")
	}
	s.cachedToken, s.expiresAt = tokens.AccessToken, tokens.Expiry(time.Now())
	return s.cachedToken, nil
}

func (p *Proxy) accessToken(ctx context.Context) (string, error) {
	return p.tokens.token(ctx, false)
}

func (p *Proxy) quotaAccessToken(ctx context.Context) (string, error) {
	return p.tokens.token(ctx, true)
}
