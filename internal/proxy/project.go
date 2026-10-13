package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type projectDiscovery struct {
	*sharedFlight[string]
}

func (p *Proxy) getProjectID(ctx context.Context, token string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p.projectMu.Lock()
	if p.cfg.ProjectID != "" {
		p.projectMu.Unlock()
		return p.cfg.ProjectID, nil
	}
	if p.projectID != "" {
		id := p.projectID
		p.projectMu.Unlock()
		return id, nil
	}
	flight := p.projectFlight
	var discoveryCtx context.Context
	if flight == nil {
		var cancel context.CancelFunc
		discoveryCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), p.finiteRPCTimeout())
		flight = &projectDiscovery{sharedFlight: newSharedFlight[string](cancel)}
		p.projectFlight = flight
	}
	flight.join()
	p.projectMu.Unlock()
	if discoveryCtx != nil {
		go p.discoverProjectFlight(discoveryCtx, token, flight)
	}
	return flight.wait(ctx, &p.projectMu, func() {
		if p.projectFlight == flight {
			p.projectFlight = nil
		}
	})
}

func (p *Proxy) discoverProjectFlight(ctx context.Context, token string, flight *projectDiscovery) {
	defer flight.cancel()
	id, err := p.discoverProject(ctx, token)
	p.projectMu.Lock()
	defer p.projectMu.Unlock()
	if p.projectFlight == flight {
		if err == nil && id != "" {
			p.projectID = id
			p.projectRevision.Add(1)
		}
		p.projectFlight = nil
	}
	flight.finish(id, err)
}

func (p *Proxy) invalidateProject(project string, revision uint64) {
	p.projectMu.Lock()
	invalidated := p.projectID == project && p.projectRevision.Load() == revision
	if invalidated {
		if p.cfg.ProjectID == "" {
			p.projectID = ""
		}
		p.projectRevision.Add(1)
	}
	p.projectMu.Unlock()
	if invalidated {
		p.invalidateGeminiModelCatalog()
	}
}

func (p *Proxy) discoverProject(ctx context.Context, token string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.finiteRPCTimeout())
	defer cancel()
	load, err := p.fetchCodeAssist(ctx, token)
	if err != nil {
		return "", err
	}
	if id := load.projectID(); id != "" {
		return id, nil
	}
	return p.pollOnboarding(ctx, token, load, defaultOnboardingAttempts, defaultOnboardingInterval)
}

func (p *Proxy) pollOnboarding(ctx context.Context, token string, load *codeAssistResponse, attempts int, interval time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.finiteRPCTimeout())
	defer cancel()
	tierID := ""
	if load != nil {
		var tiers []json.RawMessage
		_ = json.Unmarshal(load.AllowedTiers, &tiers)
		for _, raw := range tiers {
			var tier assistTier
			if json.Unmarshal(raw, &tier) != nil {
				continue
			}
			id := rawString(tier.ID)
			if id == "" {
				continue
			}
			if tierID == "" {
				tierID = id
			}
			var isDefault bool
			_ = json.Unmarshal(tier.IsDefault, &isDefault)
			if isDefault {
				tierID = id
				break
			}
		}
	}
	if tierID == "" {
		tierID = "free-tier"
	}
	payload := map[string]any{"tierId": tierID, "metadata": clientMetadata()}
	for attempt := range attempts {
		resp, err := p.postJSON(ctx, token, "/v1internal:onboardUser", "", payload)
		if err != nil {
			return "", err
		}
		var data struct {
			Done     bool               `json:"done"`
			Response codeAssistResponse `json:"response"`
		}
		err = decodeBoundedJSON(resp.Body, 1<<20, &data)
		_ = resp.Body.Close()
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", errors.New("decode Antigravity onboarding response failed")
		}
		if data.Done {
			if id := data.Response.projectID(); id != "" {
				return id, nil
			}
			return "", errors.New("Antigravity onboarding completed without a project ID")
		}
		if attempt+1 < attempts {
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", ctx.Err()
			case <-timer.C:
			}
		}
	}
	return "", errors.New("Antigravity onboarding did not complete within the polling limit")
}
