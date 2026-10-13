package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"antigravity-proxy/internal/apikey"
	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/httpjson"
	"antigravity-proxy/internal/lifecycle"
	"antigravity-proxy/internal/oauth"
	"antigravity-proxy/internal/quota"
	"antigravity-proxy/internal/status"
)

type accountRuntime struct {
	id        string
	label     string
	enabled   bool
	proxy     *Proxy
	quota     *quota.Fetcher
	status    *status.Service
	namespace string
}

type runtimeRegistry struct {
	accounts map[string]*accountRuntime
	keys     *apikey.Registry
	lastUsed sync.Map
}

func (reg *runtimeRegistry) recordKeyUsage(keyID string) {
	if keyID == "" {
		return
	}
	reg.lastUsed.Store(keyID, time.Now().UTC())
}

func (reg *runtimeRegistry) getKeyLastUsed(keyID string) *time.Time {
	if val, ok := reg.lastUsed.Load(keyID); ok {
		t := val.(time.Time)
		return &t
	}
	return nil
}

type activationFailure struct {
	cause   error
	message string
}

func (e *activationFailure) Error() string { return e.message }
func (e *activationFailure) Unwrap() error { return e.cause }

// Runtime owns account lifecycles, API key dispatch, and management routing.
type Runtime struct {
	registry      atomic.Pointer[runtimeRegistry]
	active        atomic.Pointer[accountRuntime] // alias for backward-compatible tests
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
	persist       func(string, func(*[]config.Account) error) error

	adminKeyHash string
	adminAPIKey  string
	adminHandler http.Handler
	envAccounts  map[string]bool
}

func (r *Runtime) isEnvAccount(id string) bool {
	if r.envAccounts == nil {
		return false
	}
	return r.envAccounts[id]
}

// NewRuntime constructs and initializes the multi-account runtime.
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

	envAccs := make(map[string]bool)
	if config.TokenEnvironmentOverrides() {
		envAccs["default"] = true
	}

	ownedCtx, cancel := context.WithCancel(ctx)
	r := &Runtime{
		base:          cfg,
		configPath:    configPath,
		ctx:           ownedCtx,
		cancel:        cancel,
		client:        newUpstreamClient(defaultHeaderTimeout, defaultBodyIdleTimeout),
		profileClient: profileClient,
		slots:         make(chan struct{}, cfg.MaxConcurrentGenerations),
		persist:       config.UpdateAccounts,
		adminKeyHash:  cfg.AdminKeyHash,
		adminAPIKey:   cfg.AdminAPIKey,
		envAccounts:   envAccs,
	}

	r.adminHandler = r.setupAdminMux()
	r.login = oauth.NewSessionManagerWithOptions(profileClient, r.activate)

	accounts := cfg.Accounts
	if len(accounts) == 0 && cfg.HasLegacyCredentials() {
		accounts = cfg.MigratedLegacyAccounts()
	}
	accounts = r.base.ApplyEnvironmentLayer(accounts)

	if err := r.rebuildRegistry(accounts, nil); err != nil {
		cancel()
		_ = r.login.Close()
		r.client.CloseIdleConnections()
		return nil, err
	}

	// Trigger initial polls and background profile resolution
	reg := r.registry.Load()
	if reg != nil {
		for _, acc := range reg.accounts {
			if acc.status != nil {
				acc.status.TriggerPoll()
			}
			if acc.proxy.hasCredentials() && acc.proxy.cfg.AccountID == "" {
				accountID := acc.id
				r.background.Add(1)
				go func() {
					defer r.background.Done()
					r.resolveInitialProfile(accountID)
				}()
			}
		}
	}

	return r, nil
}

func accountNamespace(acc config.Account) string {
	if acc.AccountID != "" {
		return "google:" + acc.AccountID
	}
	if acc.AccessToken == "" && acc.RefreshToken == "" {
		return "unconfigured:" + acc.ID
	}
	encoded, _ := json.Marshal([]string{acc.AccessToken, acc.RefreshToken, acc.OAuthClientID, acc.OAuthClientSecret})
	digest := sha256.Sum256(encoded)
	return "unresolved:" + hex.EncodeToString(digest[:])
}

func (r *Runtime) prepareAccount(accCfg config.Account, tokens *oauth.Tokens, previous *accountRuntime) (*accountRuntime, error) {
	proxyCfg := r.base
	proxyCfg.AccessToken = accCfg.AccessToken
	proxyCfg.RefreshToken = accCfg.RefreshToken
	proxyCfg.OAuthClientID = accCfg.OAuthClientID
	proxyCfg.OAuthClientSecret = accCfg.OAuthClientSecret
	proxyCfg.AccountID = accCfg.AccountID
	proxyCfg.AccountEmail = accCfg.AccountEmail
	proxyCfg.AccountName = accCfg.AccountName
	if accCfg.ProjectID != "" {
		proxyCfg.ProjectID = accCfg.ProjectID
	}

	p := newProxy(proxyCfg, r.client, r.slots, r.profileClient)
	if tokens != nil && proxyCfg.RefreshToken != "" {
		p.tokens.seed(tokens.AccessToken, tokens.Expiry(time.Now()))
	}

	candidate := &accountRuntime{
		id:        accCfg.ID,
		label:     accCfg.Label,
		enabled:   accCfg.Enabled,
		proxy:     p,
		quota:     p.QuotaFetcher(),
		namespace: accountNamespace(accCfg),
	}

	namespace := candidate.namespace
	if previous != nil && candidate.namespace == previous.namespace {
		candidate.status = previous.status
	} else {
		options := status.Options{
			HistoryPath:         status.AccountHistoryPath(r.base.QuotaHistoryPath, namespace),
			PollIntervalSeconds: proxyCfg.QuotaPollIntervalSeconds,
			MaxSamples:          proxyCfg.QuotaHistoryMaxSamples,
		}
		targetID := accCfg.ID
		candidate.status = status.New(options, func(ctx context.Context) (quota.Snapshot, error) {
			reg := r.registry.Load()
			if reg == nil {
				return quota.Snapshot{}, errors.New("runtime is not active")
			}
			acc := reg.accounts[targetID]
			if acc == nil || acc.namespace != namespace {
				return quota.Snapshot{}, errors.New("quota account is not active")
			}
			if acc.proxy.cfg.AccountID == "" {
				return quota.Snapshot{}, errors.New("Google account identity is not configured")
			}
			return acc.quota.Fetch(ctx)
		})
		if err := candidate.status.Start(r.ctx); err != nil {
			_ = candidate.status.Close()
			return nil, err
		}
	}

	p.StatusHandler = candidate.status
	if proxyCfg.AccountID == "" {
		accID := accCfg.ID
		p.profileResolved = func(ctx context.Context, user oauth.UserInfo) error {
			return r.promoteIdentity(ctx, accID, user)
		}
	}

	return candidate, nil
}

func accountUnchanged(prev *accountRuntime, accCfg config.Account) bool {
	if prev == nil || prev.proxy == nil {
		return false
	}
	if prev.id != accCfg.ID || prev.label != accCfg.Label || prev.enabled != accCfg.Enabled {
		return false
	}
	cfg := prev.proxy.cfg
	return cfg.AccessToken == accCfg.AccessToken &&
		cfg.RefreshToken == accCfg.RefreshToken &&
		cfg.OAuthClientID == accCfg.OAuthClientID &&
		cfg.OAuthClientSecret == accCfg.OAuthClientSecret &&
		cfg.AccountID == accCfg.AccountID &&
		cfg.AccountEmail == accCfg.AccountEmail &&
		cfg.AccountName == accCfg.AccountName &&
		cfg.ProjectID == accCfg.ProjectID
}

func accountMetaOnlyChanged(prev *accountRuntime, accCfg config.Account) bool {
	if prev == nil || prev.proxy == nil {
		return false
	}
	if prev.id != accCfg.ID {
		return false
	}
	cfg := prev.proxy.cfg
	return cfg.AccessToken == accCfg.AccessToken &&
		cfg.RefreshToken == accCfg.RefreshToken &&
		cfg.OAuthClientID == accCfg.OAuthClientID &&
		cfg.OAuthClientSecret == accCfg.OAuthClientSecret &&
		cfg.AccountID == accCfg.AccountID &&
		cfg.AccountEmail == accCfg.AccountEmail &&
		cfg.AccountName == accCfg.AccountName &&
		cfg.ProjectID == accCfg.ProjectID
}

func statusReused(prevAccounts map[string]*accountRuntime, acc *accountRuntime) bool {
	if prevAccounts == nil || acc == nil || acc.status == nil {
		return false
	}
	prev, ok := prevAccounts[acc.id]
	return ok && prev.status == acc.status
}

func (r *Runtime) rebuildRegistry(accounts []config.Account, previousReg *runtimeRegistry) error {
	newAccounts := make(map[string]*accountRuntime, len(accounts))
	bindings := make(map[string]apikey.Binding)

	var prevAccounts map[string]*accountRuntime
	if previousReg != nil {
		prevAccounts = previousReg.accounts
	}

	for _, accCfg := range accounts {
		var prev *accountRuntime
		if prevAccounts != nil {
			prev = prevAccounts[accCfg.ID]
		}
		var acc *accountRuntime
		if prev != nil && accountUnchanged(prev, accCfg) {
			acc = prev
		} else if prev != nil && accountMetaOnlyChanged(prev, accCfg) {
			acc = &accountRuntime{
				id:        prev.id,
				label:     accCfg.Label,
				enabled:   accCfg.Enabled,
				proxy:     prev.proxy,
				quota:     prev.quota,
				status:    prev.status,
				namespace: prev.namespace,
			}
		} else {
			var err error
			acc, err = r.prepareAccount(accCfg, nil, prev)
			if err != nil {
				// Clean up newly started services on error
				for _, started := range newAccounts {
					if !statusReused(prevAccounts, started) {
						_ = started.status.Close()
					}
				}
				return err
			}
		}
		newAccounts[accCfg.ID] = acc

		for _, k := range accCfg.Keys {
			bindings[k.Hash] = apikey.Binding{
				AccountID: accCfg.ID,
				KeyID:     k.ID,
			}
		}
	}

	regKeys, err := apikey.NewRegistry(bindings)
	if err != nil {
		for _, started := range newAccounts {
			if !statusReused(prevAccounts, started) {
				_ = started.status.Close()
			}
		}
		return err
	}

	newReg := &runtimeRegistry{
		accounts: newAccounts,
		keys:     regKeys,
	}
	if previousReg != nil {
		previousReg.lastUsed.Range(func(key, value any) bool {
			if keyStr, ok := key.(string); ok && regKeys.HasKeyID(keyStr) {
				newReg.lastUsed.Store(keyStr, value)
			}
			return true
		})
	}

	// Close status services for accounts that were removed or changed status instance
	if prevAccounts != nil {
		for id, oldAcc := range prevAccounts {
			if next, stillPresent := newAccounts[id]; !stillPresent || next.status != oldAcc.status {
				_ = oldAcc.status.Close()
			}
		}
	}

	r.registry.Store(newReg)

	// Set active alias for legacy/test compatibility
	if def, ok := newAccounts["default"]; ok {
		r.active.Store(def)
	} else {
		for _, acc := range newAccounts {
			r.active.Store(acc)
			break
		}
	}

	return nil
}

func (r *Runtime) reloadRegistryLocked() error {
	accounts, err := r.loadEffectiveAccounts()
	if err != nil {
		return err
	}
	return r.rebuildRegistry(accounts, r.registry.Load())
}

func (r *Runtime) loadEffectiveAccounts() ([]config.Account, error) {
	diskAccounts, err := config.ReadDiskAccounts(r.configPath)
	if err != nil {
		return nil, err
	}
	return r.base.ApplyEnvironmentLayer(diskAccounts), nil
}

func (r *Runtime) resolveInitialProfile(accountID string) {
	ctx, cancel := context.WithTimeout(r.ctx, oauth.DefaultHTTPTimeout)
	defer cancel()
	reg := r.registry.Load()
	if reg == nil {
		return
	}
	acc := reg.accounts[accountID]
	if acc == nil {
		return
	}
	token, err := acc.proxy.accessToken(ctx)
	if err != nil {
		return
	}
	user, err := oauth.FetchUserInfoWithClient(ctx, token, r.profileClient)
	if err != nil {
		return
	}
	_ = r.promoteIdentity(ctx, accountID, user)
}

func (r *Runtime) promoteIdentity(ctx context.Context, accountID string, user oauth.UserInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.ctx.Err(); err != nil {
		return err
	}

	reg := r.registry.Load()
	if reg == nil {
		return nil
	}
	acc := reg.accounts[accountID]
	if acc == nil || acc.proxy.cfg.AccountID != "" {
		return nil
	}

	cached, expires := acc.proxy.tokens.snapshot()

	accCfg := config.Account{
		ID:                acc.id,
		Label:             acc.label,
		Enabled:           acc.enabled,
		RefreshToken:      acc.proxy.cfg.RefreshToken,
		AccessToken:       acc.proxy.cfg.AccessToken,
		OAuthClientID:     acc.proxy.cfg.OAuthClientID,
		OAuthClientSecret: acc.proxy.cfg.OAuthClientSecret,
		AccountID:         user.ID,
		AccountEmail:      user.Email,
		AccountName:       user.Name,
		ProjectID:         acc.proxy.cfg.ProjectID,
	}

	candidate, err := r.prepareAccount(accCfg, nil, acc)
	if err != nil {
		return err
	}
	candidate.proxy.tokens.seed(cached, expires)

	newAccounts := make(map[string]*accountRuntime, len(reg.accounts))
	for k, v := range reg.accounts {
		if k == accountID {
			newAccounts[k] = candidate
		} else {
			newAccounts[k] = v
		}
	}
	newReg := &runtimeRegistry{
		accounts: newAccounts,
		keys:     reg.keys,
	}
	reg.lastUsed.Range(func(key, value any) bool {
		newReg.lastUsed.Store(key, value)
		return true
	})

	r.registry.Store(newReg)
	if accountID == "default" || r.active.Load() == acc {
		r.active.Store(candidate)
	}
	candidate.status.TriggerPoll()

	return nil
}

func (r *Runtime) activate(ctx context.Context, opts oauth.SessionOptions, tokens oauth.Tokens, clientID, clientSecret string, user oauth.UserInfo) (oauth.AccountConfigResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return oauth.AccountConfigResult{}, err
	}
	if err := r.ctx.Err(); err != nil {
		return oauth.AccountConfigResult{}, err
	}

	diskAccounts, err := r.readDiskAccounts()
	if err != nil {
		return oauth.AccountConfigResult{}, &activationFailure{cause: err, message: "read accounts configuration"}
	}

	// Account target resolution
	targetID := opts.AccountID
	existingIDs := make([]string, 0, len(diskAccounts))
	for _, da := range diskAccounts {
		existingIDs = append(existingIDs, da.ID)
	}

	if targetID != "" {
		// Verify Google account isn't already linked to another account
		for _, da := range diskAccounts {
			if da.AccountID == user.ID && da.ID != targetID {
				return oauth.AccountConfigResult{}, oauth.ErrGoogleAccountAlreadyLinked
			}
		}
		// Verify existing account doesn't belong to another Google user
		for _, da := range diskAccounts {
			if da.ID == targetID && da.AccountID != "" && da.AccountID != user.ID {
				if !opts.Replace {
					return oauth.AccountConfigResult{}, oauth.ErrAccountIdentityMismatch
				}
			}
		}
	} else {
		// Auto-match by Google ID or derive from email
		matched := false
		for _, da := range diskAccounts {
			if da.AccountID == user.ID {
				targetID = da.ID
				matched = true
				break
			}
		}
		if !matched {
			targetID = config.DeriveAccountID(user.Email, existingIDs)
		}
	}

	if r.isEnvAccount(targetID) {
		return oauth.AccountConfigResult{}, fmt.Errorf("account %q is managed by environment variables and cannot be replaced", targetID)
	}

	var existingAcc *config.Account
	for i := range diskAccounts {
		if diskAccounts[i].ID == targetID {
			existingAcc = &diskAccounts[i]
			break
		}
	}

	var mintedKeyID, mintedSecret string
	var newKey *config.AccountKey
	if opts.IssueKey != "" {
		mintedSecret, err = apikey.Generate()
		if err != nil {
			return oauth.AccountConfigResult{}, &activationFailure{cause: err, message: "generate API key secret"}
		}
		mintedKeyID, err = apikey.GenerateID()
		if err != nil {
			return oauth.AccountConfigResult{}, &activationFailure{cause: err, message: "generate API key id"}
		}
		newKey = &config.AccountKey{
			ID:        mintedKeyID,
			Label:     opts.IssueKey,
			Hash:      apikey.Hash(mintedSecret),
			Last4:     apikey.Last4(mintedSecret),
			CreatedAt: time.Now().UTC(),
		}
	}

	accessToken := ""
	if tokens.RefreshToken == "" {
		accessToken = tokens.AccessToken
	}

	creds := config.Credentials{
		AccessToken:       accessToken,
		RefreshToken:      tokens.RefreshToken,
		OAuthClientID:     clientID,
		OAuthClientSecret: clientSecret,
		AccountID:         user.ID,
		AccountEmail:      user.Email,
		AccountName:       user.Name,
	}

	// Prepare candidate account before writing to disk
	candidateCfg := config.Account{
		ID:      targetID,
		Label:   targetID,
		Enabled: true,
	}
	if existingAcc != nil {
		candidateCfg = *existingAcc
	}
	candidateCfg.ApplyCredentials(creds)
	reg := r.registry.Load()
	var prev *accountRuntime
	if reg != nil {
		prev = reg.accounts[targetID]
	}
	candidate, err := r.prepareAccount(candidateCfg, &tokens, prev)
	if err != nil {
		return oauth.AccountConfigResult{}, &activationFailure{cause: err, message: "could not prepare account quota history"}
	}
	abort := func() {
		if prev == nil || candidate.status != prev.status {
			_ = candidate.status.Close()
		}
	}
	if err := ctx.Err(); err != nil {
		abort()
		return oauth.AccountConfigResult{}, err
	}
	if err := r.ctx.Err(); err != nil {
		abort()
		return oauth.AccountConfigResult{}, err
	}

	writeErr := r.persist(r.configPath, func(accounts *[]config.Account) error {
		for i := range *accounts {
			if (*accounts)[i].ID == targetID {
				(*accounts)[i].ApplyCredentials(creds)
				if newKey != nil {
					(*accounts)[i].Keys = append((*accounts)[i].Keys, *newKey)
				}
				return nil
			}
		}
		var keys []config.AccountKey
		if newKey != nil {
			keys = append(keys, *newKey)
		}
		newAcc := config.Account{
			ID:      targetID,
			Label:   targetID,
			Enabled: true,
			Keys:    keys,
		}
		newAcc.ApplyCredentials(creds)
		*accounts = append(*accounts, newAcc)
		return nil
	})

	var committed *config.CommitError
	if writeErr != nil && !errors.As(writeErr, &committed) {
		abort()
		if errors.Is(writeErr, config.ErrUnloadableConfig) {
			return oauth.AccountConfigResult{}, writeErr
		}
		return oauth.AccountConfigResult{}, &activationFailure{cause: writeErr, message: "could not save account credentials"}
	}

	res := oauth.AccountConfigResult{
		AccountID: targetID,
		KeyID:     mintedKeyID,
		KeySecret: mintedSecret,
	}

	if err := r.publishAccount(candidate, prev, newKey); err != nil {
		_ = r.reloadRegistryLocked()
		return res, &config.CommitError{Err: err}
	}

	return res, writeErr
}

func (r *Runtime) publishAccount(candidate, prev *accountRuntime, newKey *config.AccountKey) error {
	reg := r.registry.Load()
	newAccounts := make(map[string]*accountRuntime)
	if reg != nil {
		for k, v := range reg.accounts {
			newAccounts[k] = v
		}
	}
	newAccounts[candidate.id] = candidate

	bindings := make(map[string]apikey.Binding)
	diskAccounts, err := r.readDiskAccounts()
	if err != nil {
		return err
	}
	for _, da := range diskAccounts {
		for _, k := range da.Keys {
			bindings[k.Hash] = apikey.Binding{AccountID: da.ID, KeyID: k.ID}
		}
	}
	if newKey != nil {
		bindings[newKey.Hash] = apikey.Binding{AccountID: candidate.id, KeyID: newKey.ID}
	}
	if r.base.APIKey != "" {
		hash := apikey.Hash(r.base.APIKey)
		if _, exists := bindings[hash]; !exists {
			bindings[hash] = apikey.Binding{AccountID: "default", KeyID: "key_env"}
		}
	}
	if reg != nil && reg.keys != nil {
		for hash, b := range reg.keys.Bindings() {
			if _, exists := bindings[hash]; !exists {
				bindings[hash] = b
			}
		}
	}

	regKeys, err := apikey.NewRegistry(bindings)
	if err != nil {
		return err
	}

	newReg := &runtimeRegistry{
		accounts: newAccounts,
		keys:     regKeys,
	}
	if reg != nil {
		reg.lastUsed.Range(func(key, value any) bool {
			if keyStr, ok := key.(string); ok && regKeys.HasKeyID(keyStr) {
				newReg.lastUsed.Store(keyStr, value)
			}
			return true
		})
	}

	r.registry.Store(newReg)
	if candidate.id == "default" || r.active.Load() == prev {
		r.active.Store(candidate)
	}
	candidate.status.TriggerPoll()

	if prev != nil && candidate.status != prev.status {
		_ = prev.status.Close()
	}
	return nil
}

func (r *Runtime) hasAnyCredentialConfigured() bool {
	reg := r.registry.Load()
	if reg == nil {
		return false
	}
	for _, acc := range reg.accounts {
		if acc.enabled && acc.proxy != nil && acc.proxy.hasCredentials() {
			return true
		}
	}
	return false
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
}

// ServeHTTP handles health, admin routes, and data routes dispatched to the account selected by API key.
func (r *Runtime) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if r.ctx.Err() != nil {
		httpjson.Write(w, http.StatusServiceUnavailable, map[string]string{"error": "runtime is closed"})
		return
	}

	// 1. Health check (public)
	if request.URL.Path == "/health" && request.Method == http.MethodGet {
		httpjson.Write(w, http.StatusOK, map[string]any{
			"status":                "ok",
			"credential_configured": r.hasAnyCredentialConfigured(),
		})
		return
	}

	// 2. Admin routes
	if request.URL.Path == "/admin" || strings.HasPrefix(request.URL.Path, "/admin/") {
		r.handleAdmin(w, request)
		return
	}

	// 3. Data routes
	reg := r.registry.Load()
	if reg == nil {
		httpjson.Write(w, http.StatusServiceUnavailable, map[string]string{"error": "runtime is not started"})
		return
	}

	presentedKey := apikey.Extract(request, true)
	var account *accountRuntime
	var keyID string

	if presentedKey == "" {
		// Zero-key loopback fallback if exactly one account is configured
		if isLoopback(r.base.Host) && len(reg.accounts) == 1 && reg.keys.Size() == 0 {
			for _, acc := range reg.accounts {
				account = acc
				break
			}
		}
		if account == nil {
			r.writeUnauthorized(w, request)
			return
		}
	} else {
		binding, ok := reg.keys.Lookup(presentedKey)
		if !ok {
			r.writeUnauthorized(w, request)
			return
		}
		account = reg.accounts[binding.AccountID]
		if account == nil {
			r.writeUnauthorized(w, request)
			return
		}
		keyID = binding.KeyID
	}

	if !account.enabled {
		if strings.HasPrefix(request.URL.Path, "/v1beta/") {
			writeGeminiError(w, http.StatusForbidden, "account is disabled")
		} else {
			httpjson.Write(w, http.StatusForbidden, map[string]string{"error": "account is disabled"})
		}
		return
	}

	reg.recordKeyUsage(keyID)
	account.proxy.serveWithAccessLogScoped(w, request, account.id, keyID)
}

func (r *Runtime) handleAdmin(w http.ResponseWriter, req *http.Request) {
	presented := apikey.Extract(req, false)
	if r.adminKeyHash == "" && r.adminAPIKey == "" {
		rejectUnreadBody(w, req)
		httpjson.Write(w, http.StatusServiceUnavailable, map[string]string{
			"error": "ADMIN_API_KEY must be configured on the server to use admin routes",
		})
		return
	}

	authorized := false
	if r.adminKeyHash != "" && apikey.Match(presented, r.adminKeyHash) {
		authorized = true
	} else if r.adminAPIKey != "" && apikey.Match(presented, r.adminAPIKey) {
		authorized = true
	}

	if !authorized {
		rejectUnreadBody(w, req)
		httpjson.Write(w, http.StatusUnauthorized, map[string]string{
			"error": "invalid admin API key",
		})
		return
	}

	r.adminHandler.ServeHTTP(w, req)
}

func (r *Runtime) writeUnauthorized(w http.ResponseWriter, req *http.Request) {
	rejectUnreadBody(w, req)
	if strings.HasPrefix(req.URL.Path, "/v1beta/") {
		writeGeminiError(w, http.StatusUnauthorized, "invalid API key")
	} else {
		httpjson.Write(w, http.StatusUnauthorized, map[string]string{"error": "invalid API key"})
	}
}

// Close gracefully stops the runtime, background workers, and quota polling.
func (r *Runtime) Close() error {
	return r.closer.Close(func() error {
		r.cancel()
		loginErr := r.login.Close()
		r.background.Wait()
		r.mu.Lock()
		var statusErrs []error
		reg := r.registry.Load()
		if reg != nil {
			for _, acc := range reg.accounts {
				if acc.status != nil {
					if err := acc.status.Close(); err != nil {
						statusErrs = append(statusErrs, err)
					}
				}
			}
		}
		r.mu.Unlock()
		r.client.CloseIdleConnections()
		return errors.Join(append([]error{loginErr}, statusErrs...)...)
	})
}
