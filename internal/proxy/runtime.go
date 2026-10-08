package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	closeOnce     sync.Once
	closeErr      error
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
	if cfg.MaxConcurrentGenerations <= 0 {
		cfg.MaxConcurrentGenerations = config.DefaultMaxConcurrentGenerations
	}
	if profileClient == nil {
		profileClient = &http.Client{Timeout: 30 * time.Second}
	}
	ownedCtx, cancel := context.WithCancel(ctx)
	r := &Runtime{
		base: cfg, configPath: configPath, ctx: ownedCtx, cancel: cancel,
		client: newUpstreamClient(5*time.Minute, 5*time.Minute), profileClient: profileClient,
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
		p.cachedToken = tokens.AccessToken
		seconds := tokens.ExpiresIn
		if seconds <= 0 {
			seconds = 3600
		}
		p.tokenExpiresAt = time.Now().Add(time.Duration(seconds) * time.Second)
	}
	candidate := &accountRuntime{proxy: p, quota: p.QuotaFetcher(), namespace: accountNamespace(cfg)}
	namespace := candidate.namespace
	if previous != nil && candidate.namespace == previous.namespace {
		candidate.status = previous.status
	} else {
		statusCfg := cfg
		statusCfg.QuotaHistoryPath = status.AccountHistoryPath(r.base.QuotaHistoryPath, namespace)
		candidate.status = status.NewService(statusCfg, func(ctx context.Context) (quota.Snapshot, error) {
			current := r.active.Load()
			if current == nil || current.namespace != namespace {
				return quota.Snapshot{}, errors.New("quota account is not active")
			}
			if current.proxy.cfg.AccountID == "" {
				return quota.Snapshot{}, errors.New("Google account identity is not configured")
			}
			return current.quota.Fetch(ctx)
		})
		if err := candidate.status.Prepare(); err != nil {
			return nil, err
		}
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
	ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
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
	initial.proxy.tokenMu.Lock()
	cached, expires := initial.proxy.cachedToken, initial.proxy.tokenExpiresAt
	initial.proxy.tokenMu.Unlock()
	candidate, err := r.prepare(cfg, nil, initial)
	if err != nil {
		return err
	}
	// This expiry is already known, not a missing provider lifetime. Never
	// extend an expired startup token when publishing its recovered identity.
	candidate.proxy.cachedToken = cached
	candidate.proxy.tokenExpiresAt = expires
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
	if tokens.AccessToken == "" || strings.TrimSpace(user.ID) == "" || strings.TrimSpace(user.Email) == "" {
		return errors.New("Google authorization did not return account credentials and identity")
	}
	cfg := r.base
	cfg.AccessToken, cfg.RefreshToken = "", tokens.RefreshToken
	if tokens.RefreshToken == "" {
		cfg.AccessToken = tokens.AccessToken
	}
	cfg.OAuthClientID, cfg.OAuthClientSecret = clientID, clientSecret
	cfg.AccountID, cfg.AccountEmail, cfg.AccountName = user.ID, user.Email, user.Name
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
	writeErr := r.persist(r.configPath, config.Credentials{
		AccessToken: cfg.AccessToken, RefreshToken: cfg.RefreshToken,
		OAuthClientID: clientID, OAuthClientSecret: clientSecret,
		AccountID: user.ID, AccountEmail: user.Email, AccountName: user.Name,
	})
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
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "runtime is not started"})
		return
	}
	current.proxy.ServeHTTP(w, request)
}

func (r *Runtime) Close() error {
	r.closeOnce.Do(func() {
		r.cancel()
		loginErr := r.login.Close()
		r.background.Wait()
		r.mu.Lock()
		if current := r.active.Load(); current != nil {
			r.closeErr = current.status.Close()
		}
		r.mu.Unlock()
		r.closeErr = errors.Join(loginErr, r.closeErr)
		r.client.CloseIdleConnections()
	})
	return r.closeErr
}

func (r *Runtime) handleLogin(w http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		address, expires, err := r.login.Begin(request.Context())
		if err != nil {
			writeLoginError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"login_url": address, "expires_at": expires.UTC().Format(time.RFC3339)})
	case http.MethodPost:
		var submission struct {
			Code  string `json:"code"`
			State string `json:"state"`
			URL   string `json:"url"`
		}
		request.Body = http.MaxBytesReader(w, request.Body, 64<<10)
		defer request.Body.Close()
		decoder := json.NewDecoder(request.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&submission); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid login JSON"})
			return
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "login requires one JSON object"})
			return
		}
		code, state := strings.TrimSpace(submission.Code), strings.TrimSpace(submission.State)
		if submission.URL != "" {
			if code != "" || state != "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provide code/state or callback URL, not both"})
				return
			}
			callback, err := url.Parse(submission.URL)
			if err != nil || callback.Scheme != "http" || callback.Hostname() != "127.0.0.1" || callback.Path != "/oauth-callback" || callback.User != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid loopback callback URL"})
				return
			}
			code, state = callback.Query().Get("code"), callback.Query().Get("state")
		}
		if code == "" || state == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "both code and state are required"})
			return
		}
		user, err := r.login.Complete(request.Context(), code, state)
		if err != nil {
			writeLoginError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "configured", "email": user.Email, "name": user.Name})
	default:
		rejectUnreadBody(w, request)
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func writeLoginError(w http.ResponseWriter, err error) {
	var commit *config.CommitError
	if errors.As(err, &commit) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "account activated, but credential durability could not be confirmed", "credential_configured": true, "durability_uncertain": true})
		return
	}
	var activation *activationFailure
	if errors.As(err, &activation) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": activation.message})
		return
	}
	code, message := http.StatusBadGateway, "failed to complete Google authorization"
	switch {
	case errors.Is(err, oauth.ErrInvalidState):
		code, message = http.StatusBadRequest, "invalid OAuth state"
	case errors.Is(err, oauth.ErrLoginInProgress):
		code, message = http.StatusConflict, "login completion already in progress"
	case errors.Is(err, oauth.ErrSessionAlreadyUsed):
		code, message = http.StatusConflict, "OAuth session already used"
	case errors.Is(err, oauth.ErrSessionExpired):
		code, message = http.StatusGone, "OAuth session expired"
	case errors.Is(err, oauth.ErrTokenEnvironment):
		code, message = http.StatusConflict, "remove token environment overrides to use /config/login"
	case errors.Is(err, oauth.ErrSessionManagerClosed):
		code, message = http.StatusServiceUnavailable, "login is closed"
	case errors.Is(err, context.Canceled):
		code, message = http.StatusServiceUnavailable, "login canceled"
	}
	writeJSON(w, code, map[string]string{"error": message})
}
