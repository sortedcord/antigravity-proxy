package proxy

import (
	"antigravity-proxy/internal/quota"
	"context"
	"net/http"
)

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
		PostSummary: func(ctx context.Context, token, project string) (*http.Response, error) {
			payload := make(map[string]string, 1)
			if project != "" {
				payload["project"] = project
			}
			return p.postJSON(ctx, token, quota.SummaryPath, project, payload)
		},
	})
}
