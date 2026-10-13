package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"antigravity-proxy/internal/httpjson"
	"antigravity-proxy/internal/lifecycle"

	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/oauth"
	"antigravity-proxy/internal/quota"
	"antigravity-proxy/internal/status"
)

type accountRuntime struct {
	proxy     *Proxy
	quota     *quota.Fetcher
	status    *status.Service
	namespace string
}

type activationFailure struct {
	cause   error
	message string
}

func (e *activationFailure) Error() string { return e.message }
func (e *activationFailure) Unwrap() error { return e.cause }

// Runtime owns login and publishes complete account snapshots. Requests and polls
// retain one snapshot; transport pools and generation capacity remain server-wide.
type Runtime struct {
	active        atomic.Pointer[accountRuntime]
	mu            sync.Mutex
	base          config.Config
	configPath    string
	ctx           context.Context
	cancel        context.CancelFunc
	client        *http.Client
	profileClient *http.Client
	slots         chan struct{}
	login         *oauth.SessionManager
	background    sync.WaitGroup
	closer        lifecycle.Closer
	persist       func(string, config.Credentials) error
}

func NewRuntime(ctx context.Context, cfg config.Config, configPath string) (*Runtime, error) {
	return newRuntime(ctx, cfg, configPath, nil)
}

func newRuntime(ctx context.Context, cfg config.Config, configPath string, profileClient *http.Client) (*Runtime, error) {
	if ctx == nil || configPath == "" {
		return nil, errors.New("runtime requires a lifecycle context and credential persistence path")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg = cfg.WithProxyDefaults()
	if profileClient == nil {
		profileClient = &http.Client{Timeout: oauth.DefaultHTTPTimeout}
	}
	ownedCtx, cancel := context.WithCancel(ctx)
	r := &Runtime{
		base: cfg, configPath: configPath, ctx: ownedCtx, cancel: cancel,
		client: newUpstreamClient(defaultHeaderTimeout, defaultBodyIdleTimeout), profileClient: profileClient,
		slots: make(chan struct{}, cfg.MaxConcurrentGenerations), persist: config.UpdateCredentials,
	}
	r.login = oauth.NewSessionManagerWithClient(profileClient, r.activate)
	initial, err := r.prepare(cfg, nil, nil)
	if err != nil {
		cancel()
		r.login.Close()
		r.client.CloseIdleConnections()
		return nil, err
	}
	r.active.Store(initial)
	initial.status.TriggerPoll()
	if initial.proxy.hasCredentials() && cfg.AccountID == "" {
		r.background.Add(1)
		go func() { defer r.background.Done(); r.resolveInitialProfile(initial) }()
	}
	return r, nil
}

func accountNamespace(cfg config.Config) string {
	if cfg.AccountID != "" {
		return "google:" + cfg.AccountID
	}
	if cfg.AccessToken == "" && cfg.RefreshToken == "" {
		return "unconfigured"
	}
	// Unattributed credentials never share a legacy journal with another account.
	encoded, _ := json.Marshal([]string{cfg.AccessToken, cfg.RefreshToken, cfg.OAuthClientID, cfg.OAuthClientSecret})
	digest := sha256.Sum256(encoded)
	return "unresolved:" + hex.EncodeToString(digest[:])
}

func (r *Runtime) prepare(cfg config.Config, tokens *oauth.Tokens, previous *accountRuntime) (*accountRuntime, error) {
	p := newProxy(cfg, r.client, r.slots, r.profileClient)
	if tokens != nil && cfg.RefreshToken != "" {
		p.tokens.seed(tokens.AccessToken, tokens.Expiry(time.Now()))
	}
	candidate := &accountRuntime{proxy: p, quota: p.QuotaFetcher(), namespace: accountNamespace(cfg)}
	namespace := candidate.namespace
	if previous != nil && candidate.namespace == previous.namespace {
		candidate.status = previous.status
	} else {
		options := status.Options{
			HistoryPath:         status.AccountHistoryPath(r.base.QuotaHistoryPath, namespace),
			PollIntervalSeconds: cfg.QuotaPollIntervalSeconds,
			MaxSamples:          cfg.QuotaHistoryMaxSamples,
		}
		candidate.status = status.New(options, func(ctx context.Context) (quota.Snapshot, error) {
			current := r.active.Load()
			if current == nil || current.namespace != namespace {
				return quota.Snapshot{}, errors.New("quota account is not active")
			}
			if current.proxy.cfg.AccountID == "" {
				return quota.Snapshot{}, errors.New("Google account identity is not configured")
			}
			return current.quota.Fetch(ctx)
		})
		if err := candidate.status.Start(r.ctx); err != nil {
			_ = candidate.status.Close()
			return nil, err
		}
	}
	p.StatusHandler = candidate.status
	p.LoginHandler = http.HandlerFunc(r.handleLogin)
	if cfg.AccountID == "" {
		p.profileResolved = func(ctx context.Context, user oauth.UserInfo) error {
			return r.promoteIdentity(ctx, candidate, user)
		}
	}
	return candidate, nil
}

func (r *Runtime) resolveInitialProfile(initial *accountRuntime) {
	ctx, cancel := context.WithTimeout(r.ctx, oauth.DefaultHTTPTimeout)
	defer cancel()
	token, err := initial.proxy.accessToken(ctx)
	if err != nil {
		return
	}
	user, err := oauth.FetchUserInfoWithClient(ctx, token, r.profileClient)
	if err != nil {
		return
	}
	_ = r.promoteIdentity(ctx, initial, user)
}

func (r *Runtime) promoteIdentity(ctx context.Context, initial *accountRuntime, user oauth.UserInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if r.active.Load() != initial {
		return nil
	}
	cfg := initial.proxy.cfg
	cfg.AccountID, cfg.AccountEmail, cfg.AccountName = user.ID, user.Email, user.Name
	cached, expires := initial.proxy.tokens.snapshot()
	candidate, err := r.prepare(cfg, nil, initial)
	if err != nil {
		return err
	}
	// This expiry is already known, not a missing provider lifetime. Never
	// extend an expired startup token when publishing its recovered identity.
	candidate.proxy.tokens.seed(cached, expires)
	r.publish(candidate, initial)
	return nil
}

func (r *Runtime) activate(ctx context.Context, tokens oauth.Tokens, clientID, clientSecret string, user oauth.UserInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.ctx.Err(); err != nil {
		return err
	}
	credentials, err := oauth.LoginCredentials(tokens, clientID, clientSecret, user)
	if err != nil {
		return err
	}
	cfg := r.base
	cfg.AccessToken, cfg.RefreshToken = credentials.AccessToken, credentials.RefreshToken
	cfg.OAuthClientID, cfg.OAuthClientSecret = credentials.OAuthClientID, credentials.OAuthClientSecret
	cfg.AccountID, cfg.AccountEmail, cfg.AccountName = credentials.AccountID, credentials.AccountEmail, credentials.AccountName
	previous := r.active.Load()
	candidate, err := r.prepare(cfg, &tokens, previous)
	if err != nil {
		return &activationFailure{cause: err, message: "could not prepare account quota history"}
	}
	abort := func() {
		if candidate.status != previous.status {
			_ = candidate.status.Close()
		}
	}
	if err := ctx.Err(); err != nil {
		abort()
		return err
	}
	if err := r.ctx.Err(); err != nil {
		abort()
		return err
	}
	writeErr := r.persist(r.configPath, credentials)
	var committed *config.CommitError
	if writeErr != nil && !errors.As(writeErr, &committed) {
		abort()
		return &activationFailure{cause: writeErr, message: "could not save account credentials"}
	}
	// A committed rename cannot be undone by caller cancellation. Publish exactly
	// the credentials now on disk, including a durability-uncertain commit.
	r.publish(candidate, previous)
	return writeErr
}

func (r *Runtime) publish(candidate, previous *accountRuntime) {
	r.active.Store(candidate)
	candidate.status.TriggerPoll()
	if previous != nil && candidate.status != previous.status {
		_ = previous.status.Close()
	}
}

func (r *Runtime) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	current := r.active.Load()
	if current == nil || r.ctx.Err() != nil {
		httpjson.Write(w, http.StatusServiceUnavailable, map[string]string{"error": "runtime is not started"})
		return
	}
	current.proxy.ServeHTTP(w, request)
}

func (r *Runtime) Close() error {
	return r.closer.Close(func() error {
		r.cancel()
		loginErr := r.login.Close()
		r.background.Wait()
		r.mu.Lock()
		var statusErr error
		if current := r.active.Load(); current != nil {
			statusErr = current.status.Close()
		}
		r.mu.Unlock()
		r.client.CloseIdleConnections()
		return errors.Join(loginErr, statusErr)
	})
}
