package proxy

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/oauth"
)

// Proxy serves native Gemini requests and the raw Antigravity model catalog
// for one Google account. Its token and project caches support concurrent handlers.
type Proxy struct {
	cfg        config.Config
	client     *http.Client
	rpcTimeout time.Duration

	tokens *tokenSource

	projectMu       sync.Mutex
	projectID       string
	projectRevision atomic.Uint64
	projectFlight   *projectDiscovery

	generationSlots chan struct{}
	catalogMu       sync.Mutex
	catalog         modelCatalogCache
	catalogEpoch    uint64

	accountMu          sync.RWMutex
	accountEmail       string
	accountName        string
	accountID          string
	subscription       *SubscriptionInfo
	subscriptionStatus string

	profileClient   *http.Client
	profileResolved func(context.Context, oauth.UserInfo) error

	// Handlers are wired before publication by the runtime coordinator.
	StatusHandler http.Handler
	LoginHandler  http.Handler
}

// New creates a Gemini upstream proxy with cfg without contacting Google.
// Wire StatusHandler before serving quota routes. Direct callers receive defaults
// for optional settings normally populated by config.Load.
func New(cfg config.Config) *Proxy {
	return newProxy(cfg, nil, nil, nil)
}

func newProxy(cfg config.Config, client *http.Client, slots chan struct{}, profileClient *http.Client) *Proxy {
	cfg = cfg.WithProxyDefaults()
	if client == nil {
		client = newUpstreamClient(defaultHeaderTimeout, defaultBodyIdleTimeout)
	}
	if slots == nil {
		slots = make(chan struct{}, cfg.MaxConcurrentGenerations)
	}
	if profileClient == nil {
		profileClient = &http.Client{Timeout: oauth.DefaultHTTPTimeout}
	}
	p := &Proxy{
		cfg:             cfg,
		client:          client,
		rpcTimeout:      defaultRPCTimeout,
		projectID:       cfg.ProjectID,
		generationSlots: slots,
		accountID:       cfg.AccountID,
		accountEmail:    cfg.AccountEmail,
		accountName:     cfg.AccountName,
		profileClient:   profileClient,
	}
	p.tokens = &tokenSource{cfg: &p.cfg, client: profileClient}
	return p
}

func (p *Proxy) hasCredentials() bool {
	return p.cfg.AccessToken != "" || p.cfg.RefreshToken != ""
}
