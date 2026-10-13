package proxy

import (
	"context"
	"errors"

	"antigravity-proxy/internal/oauth"
)

// SubscriptionInfo describes an authoritative tier returned by the provider.
type SubscriptionInfo struct {
	ID          string `json:"id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	SourceField string `json:"source_field,omitempty"`
}

// AccountDetails describes the authenticated Google account and tier.
type AccountDetails struct {
	CredentialConfigured bool              `json:"credential_configured"`
	Email                string            `json:"email,omitempty"`
	Name                 string            `json:"name,omitempty"`
	Subscription         *SubscriptionInfo `json:"subscription"`
	SubscriptionStatus   string            `json:"subscription_status"`
}

// AccountDetails returns account information including email, name, and tier.
func (p *Proxy) AccountDetails(ctx context.Context) (AccountDetails, error) {
	if !p.hasCredentials() {
		return AccountDetails{
			CredentialConfigured: false,
			Subscription:         nil,
			SubscriptionStatus:   "not_reported",
		}, errors.New("no Google credential configured")
	}

	token, err := p.accessToken(ctx)
	if err != nil {
		return AccountDetails{
			CredentialConfigured: true,
			Subscription:         nil,
			SubscriptionStatus:   "unavailable",
		}, err
	}

	p.accountMu.RLock()
	email := p.accountEmail
	name := p.accountName
	accountID := p.accountID
	sub := p.subscription
	subStatus := p.subscriptionStatus
	p.accountMu.RUnlock()

	if email == "" || accountID == "" {
		userInfo, err := oauth.FetchUserInfoWithClient(ctx, token, p.profileClient)
		if err != nil {
			return AccountDetails{}, err
		}
		p.accountMu.Lock()
		p.accountEmail = userInfo.Email
		p.accountName = userInfo.Name
		p.accountID = userInfo.ID
		email = userInfo.Email
		name = userInfo.Name
		p.accountMu.Unlock()
	}
	if p.cfg.AccountID == "" && p.profileResolved != nil {
		p.accountMu.RLock()
		identity := oauth.UserInfo{ID: p.accountID, Email: p.accountEmail, Name: p.accountName}
		p.accountMu.RUnlock()
		if err := p.profileResolved(ctx, identity); err != nil {
			return AccountDetails{}, err
		}
	}

	if subStatus == "" || subStatus == "unavailable" {
		tier, status, _ := p.fetchSubscriptionTier(ctx, token)
		p.accountMu.Lock()
		p.subscription = tier
		p.subscriptionStatus = status
		sub = tier
		subStatus = status
		p.accountMu.Unlock()
	}

	return AccountDetails{
		CredentialConfigured: true,
		Email:                email,
		Name:                 name,
		Subscription:         sub,
		SubscriptionStatus:   subStatus,
	}, nil
}

func (p *Proxy) fetchSubscriptionTier(ctx context.Context, token string) (*SubscriptionInfo, string, error) {
	load, err := p.fetchCodeAssist(ctx, token)
	if err != nil {
		return nil, "unavailable", err
	}
	tier, status := load.subscriptionTier()
	return tier, status, nil
}
