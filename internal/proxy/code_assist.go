package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Raw optional fields preserve the provider's permissive metadata contract:
// malformed/unknown tiers never hide an otherwise usable project identifier.
type codeAssistResponse struct {
	Project      json.RawMessage `json:"cloudaicompanionProject"`
	AllowedTiers json.RawMessage `json:"allowedTiers"`
	PaidTier     json.RawMessage `json:"paidTier"`
	CurrentTier  json.RawMessage `json:"currentTier"`
}

type assistTier struct {
	ID        json.RawMessage `json:"id"`
	Name      json.RawMessage `json:"name"`
	IsDefault json.RawMessage `json:"isDefault"`
}

func rawString(value json.RawMessage) string {
	var text string
	_ = json.Unmarshal(value, &text)
	return text
}

func (load codeAssistResponse) projectID() string {
	if id := rawString(load.Project); id != "" {
		return strings.TrimSpace(id)
	}
	var project struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(load.Project, &project)
	return strings.TrimSpace(project.ID)
}

func (load codeAssistResponse) subscriptionTier() (*SubscriptionInfo, string) {
	for _, field := range []struct {
		name string
		raw  json.RawMessage
	}{
		{name: "paidTier", raw: load.PaidTier},
		{name: "currentTier", raw: load.CurrentTier},
	} {
		var tier assistTier
		if json.Unmarshal(field.raw, &tier) != nil {
			continue
		}
		id, name := rawString(tier.ID), rawString(tier.Name)
		if id != "" || name != "" {
			return &SubscriptionInfo{ID: id, DisplayName: name, SourceField: field.name}, "known"
		}
	}
	return nil, "not_reported"
}

func (p *Proxy) fetchCodeAssist(ctx context.Context, token string) (*codeAssistResponse, error) {
	resp, err := p.postJSON(ctx, token, "/v1internal:loadCodeAssist", "", struct {
		Metadata clientInfo `json:"metadata"`
		Mode     int        `json:"mode"`
	}{Metadata: clientMetadata(), Mode: 1})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var load codeAssistResponse
	if err := decodeBoundedJSON(resp.Body, 1<<20, &load); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("decode Antigravity project discovery response failed")
	}
	// Discovery already observed authoritative tier metadata; account status
	// should reuse it rather than repeat loadCodeAssist just to read the tier.
	tier, status := load.subscriptionTier()
	p.accountMu.Lock()
	p.subscription, p.subscriptionStatus = tier, status
	p.accountMu.Unlock()
	return &load, nil
}

func decodeBoundedJSON(reader io.Reader, limit int64, value any) error {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > limit {
		return errors.New("JSON response exceeds size limit")
	}
	return json.Unmarshal(body, value)
}
