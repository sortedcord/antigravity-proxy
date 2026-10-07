package proxy

import (
	"context"

	"antigravity-proxy/internal/quota"
)

// quotaAccessToken shares native cache and refresh behavior but never waits for
// an unrelated OAuth refresh owner. A busy owner causes this poll to fail;
// subsequent scheduled polls can use the owner's refreshed cache.
func (p *Proxy) quotaAccessToken(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p.cfg.AccessToken != "" || p.cfg.RefreshToken == "" {
		return p.accessToken(ctx)
	}
	if !p.tokenMu.TryLock() {
		return "", quota.ErrAuthenticationBusy
	}
	defer p.tokenMu.Unlock()
	return p.accessTokenLocked(ctx)
}

// quotaProjectID only inspects the existing account cache. Native project
// discovery never delays quota polling and is never initiated by a poll.
func (p *Proxy) quotaProjectID() string {
	projectID := p.cfg.ProjectID
	if projectID == "" && p.projectMu.TryLock() {
		projectID = p.projectID
		p.projectMu.Unlock()
	}
	return projectID
}

// QuotaFetcher shares this proxy's account caches and HTTP client. It owns no
// polling lifecycle; the caller supplies Fetch to its status service.
func (p *Proxy) QuotaFetcher() *quota.Fetcher {
	return quota.NewFetcher(quota.Transport{
		AccessToken: p.quotaAccessToken,
		ProjectID:   p.quotaProjectID,
		Post:        p.postToAntigravity,
	})
}
