package oauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"
	"unicode"

	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/lifecycle"
)

const (
	oauthUserInfoURL = "https://www.googleapis.com/oauth2/v2/userinfo"
	sessionLifetime  = 5 * time.Minute
)

var (
	ErrInvalidState               = errors.New("invalid or expired OAuth state")
	ErrLoginInProgress            = errors.New("login completion already in progress")
	ErrSessionAlreadyUsed         = errors.New("OAuth session already used")
	ErrSessionExpired             = errors.New("OAuth session expired")
	ErrSessionManagerClosed       = errors.New("OAuth session manager closed")
	ErrTokenEnvironment           = errors.New("remove token environment overrides to use login")
	ErrSessionNotFound            = errors.New("login session not found")
	ErrAccountIdentityMismatch    = errors.New("account is already linked to a different Google account; pass replace: true to overwrite")
	ErrGoogleAccountAlreadyLinked = errors.New("Google account is already linked to another proxy account")
)

// SessionOptions controls account selection and optional key issuance for an OAuth session.
type SessionOptions struct {
	AccountID string `json:"account_id,omitempty"`
	Replace   bool   `json:"replace,omitempty"`
	IssueKey  string `json:"issue_key,omitempty"`
}

// SessionInfo describes a created login session.
type SessionInfo struct {
	ID        string    `json:"id"`
	AuthURL   string    `json:"login_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// AccountConfigResult describes the result of committing an account during login.
type AccountConfigResult struct {
	AccountID string
	KeyID     string
	KeySecret string
}

// OnAccountConfiguredWithOptionsFunc is the extended hook called when authorization succeeds.
type OnAccountConfiguredWithOptionsFunc func(ctx context.Context, opts SessionOptions, tokens Tokens, clientID, clientSecret string, user UserInfo) (AccountConfigResult, error)

// SessionStatus reports the state of a login session.
type SessionStatus struct {
	ID        string     `json:"id"`
	Status    string     `json:"status"`
	AuthURL   string     `json:"login_url,omitempty"`
	ExpiresAt time.Time  `json:"expires_at"`
	AccountID string     `json:"account_id,omitempty"`
	Email     string     `json:"email,omitempty"`
	Name      string     `json:"name,omitempty"`
	Key       *IssuedKey `json:"key,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// IssuedKey contains a newly generated API key secret for one-time retrieval.
type IssuedKey struct {
	ID     string `json:"id"`
	Label  string `json:"label,omitempty"`
	Secret string `json:"secret"`
}

// UserInfo contains Google account details returned by userinfo.
type UserInfo struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	Name          string `json:"name"`
	VerifiedEmail bool   `json:"verified_email"`
}

// FetchUserInfo retrieves and validates the user's identity from Google.
func FetchUserInfo(ctx context.Context, accessToken string) (UserInfo, error) {
	return FetchUserInfoWithClient(ctx, accessToken, &http.Client{Timeout: defaultUserInfoTimeout})
}

// FetchUserInfoWithClient retrieves a validated identity using the supplied client.
func FetchUserInfoWithClient(ctx context.Context, accessToken string, client *http.Client) (UserInfo, error) {
	if client == nil {
		client = &http.Client{Timeout: defaultUserInfoTimeout}
	}
	return fetchUserInfo(ctx, client, accessToken)
}

func fetchUserInfo(ctx context.Context, client *http.Client, accessToken string) (UserInfo, error) {
	if strings.TrimSpace(accessToken) == "" {
		return UserInfo{}, errors.New("Google returned no access token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, oauthUserInfoURL, nil)
	if err != nil {
		return UserInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return UserInfo{}, errors.New("fetch user info: request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return UserInfo{}, fmt.Errorf("fetch user info: Google returned HTTP %d (%s)", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	var info UserInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return UserInfo{}, errors.New("fetch user info: invalid JSON response")
	}
	if err := validateUserInfo(info); err != nil {
		return UserInfo{}, err
	}
	return info, nil
}

func validateUserInfo(info UserInfo) error {
	if info.ID == "" || strings.IndexFunc(info.ID, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return errors.New("Google returned an invalid account ID")
	}
	address, err := mail.ParseAddress(info.Email)
	if err != nil || address.Address != info.Email || address.Name != "" {
		return errors.New("Google returned an invalid account email")
	}
	return nil
}

type sessionStatus string

const (
	sessionPending    sessionStatus = "pending"
	sessionExchanging sessionStatus = "exchanging"
	sessionCompleted  sessionStatus = "completed"
	sessionFailed     sessionStatus = "failed"
	sessionExpired    sessionStatus = "expired"
)

// loginSession represents one OAuth attempt. Terminal attempts cannot be replayed.
type loginSession struct {
	id           string
	opts         SessionOptions
	state        string
	verifier     string
	redirectURI  string
	clientID     string
	clientSecret string
	authURL      string
	expiresAt    time.Time

	status       sessionStatus
	done         chan struct{}
	result       error
	resultUser   UserInfo
	resultConfig AccountConfigResult
	listener     net.Listener
	server       *http.Server
	expiryTimer  *time.Timer
}

// OnAccountConfiguredFunc prepares, persists, and activates an authenticated account.
// It must honor ctx before committing. Close cancels ctx and joins this hook.
type OnAccountConfiguredFunc func(ctx context.Context, tokens Tokens, clientID, clientSecret string, user UserInfo) error

// SessionManager owns authorization resources, never configuration persistence.
type SessionManager struct {
	mu           sync.Mutex
	active       *loginSession
	sessions     map[string]*loginSession
	onConfigured OnAccountConfiguredWithOptionsFunc
	client       *http.Client
	ctx          context.Context
	cancel       context.CancelFunc
	closed       bool
	closer       lifecycle.Closer
	// CLI login can persist replacements even with token environment overrides;
	// the management API cannot activate them while those overrides are present.
	cli  bool
	work sync.WaitGroup
}

func NewSessionManager(onConfigured OnAccountConfiguredFunc) *SessionManager {
	return NewSessionManagerWithClient(&http.Client{Timeout: DefaultHTTPTimeout}, onConfigured)
}

// NewSessionManagerWithClient uses client for both token exchange and userinfo.
func NewSessionManagerWithClient(client *http.Client, onConfigured OnAccountConfiguredFunc) *SessionManager {
	var adapted OnAccountConfiguredWithOptionsFunc
	if onConfigured != nil {
		adapted = func(ctx context.Context, opts SessionOptions, tokens Tokens, clientID, clientSecret string, user UserInfo) (AccountConfigResult, error) {
			err := onConfigured(ctx, tokens, clientID, clientSecret, user)
			return AccountConfigResult{}, err
		}
	}
	return NewSessionManagerWithOptions(client, adapted)
}

// NewSessionManagerWithOptions configures the session manager with the extended activation hook.
func NewSessionManagerWithOptions(client *http.Client, onConfigured OnAccountConfiguredWithOptionsFunc) *SessionManager {
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &SessionManager{
		client:       client,
		onConfigured: onConfigured,
		ctx:          ctx,
		cancel:       cancel,
		sessions:     make(map[string]*loginSession),
	}
}

func tokenOverrideError() error {
	if config.TokenEnvironmentOverrides() {
		return ErrTokenEnvironment
	}
	return nil
}

// BeginWithOptions initiates or reuses a pending OAuth authorization session with options.
func (m *SessionManager) BeginWithOptions(ctx context.Context, opts SessionOptions) (SessionInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return SessionInfo{}, ErrSessionManagerClosed
	}
	if err := ctx.Err(); err != nil {
		return SessionInfo{}, err
	}
	if !m.cli {
		if err := tokenOverrideError(); err != nil {
			return SessionInfo{}, err
		}
	}
	now := time.Now()
	// Prune old expired sessions to prevent unbounded growth
	for id, s := range m.sessions {
		if now.After(s.expiresAt.Add(time.Hour)) {
			delete(m.sessions, id)
		}
	}
	if m.active != nil {
		if m.active.status == sessionExchanging {
			return SessionInfo{}, ErrLoginInProgress
		}
		if m.active.status == sessionPending && now.Before(m.active.expiresAt) {
			if m.active.opts == opts {
				return SessionInfo{ID: m.active.id, AuthURL: m.active.authURL, ExpiresAt: m.active.expiresAt}, nil
			}
			return SessionInfo{}, ErrLoginInProgress
		}
		m.cleanupSessionLocked(m.active)
		m.active = nil
	}
	clientID, clientSecret, err := resolveLoginCredentials()
	if err != nil {
		return SessionInfo{}, err
	}
	listener, callbackPort, err := listenOAuthCallbackPort()
	if err != nil {
		return SessionInfo{}, err
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/oauth-callback", callbackPort)
	authReq, err := buildAuthorizationRequest(clientID, redirectURI)
	if err != nil {
		_ = listener.Close()
		return SessionInfo{}, err
	}
	var sidBuf [8]byte
	if _, err := rand.Read(sidBuf[:]); err != nil {
		_ = listener.Close()
		return SessionInfo{}, err
	}
	sessionID := "ls_" + hex.EncodeToString(sidBuf[:])

	sess := &loginSession{
		id: sessionID, opts: opts,
		state: authReq.state, verifier: authReq.verifier, redirectURI: redirectURI,
		clientID: clientID, clientSecret: clientSecret, authURL: authReq.authURL,
		expiresAt: now.Add(sessionLifetime), status: sessionPending, listener: listener,
		done: make(chan struct{}),
	}
	if m.sessions == nil {
		m.sessions = make(map[string]*loginSession)
	}
	m.sessions[sessionID] = sess
	m.active = sess
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth-callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Query().Get("state") != sess.state {
			http.Error(w, "OAuth state mismatch", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("error") != "" {
			m.failPending(sess)
			http.Error(w, "Authorization failed. You may close this tab.", http.StatusBadRequest)
			return
		}
		code := r.URL.Query().Get("code")
		// The CLI historically ignores empty stray callbacks while waiting.
		// Management completions consume a matching state even on failure.
		if m.cli && strings.TrimSpace(code) == "" {
			http.Error(w, "missing authorization code", http.StatusBadRequest)
			return
		}
		user, _, err := m.CompleteWithSessionID(r.Context(), sess.id, code, sess.state)
		if err != nil {
			var committed *config.CommitError
			if errors.As(err, &committed) {
				http.Error(w, "Authorization saved and activated, but durability could not be confirmed. Check proxy status before retrying.", http.StatusInternalServerError)
				return
			}
			// Hook errors may contain implementation details; never echo them to a browser.
			http.Error(w, "Authorization failed. Start a new login attempt.", http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprintf(w, "Authorization complete. Logged in as %s (%s). You may close this tab and return to the proxy.\n", user.Name, user.Email)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	sess.server = server
	m.active = sess
	m.work.Add(1)
	sess.expiryTimer = time.AfterFunc(sessionLifetime, func() {
		defer m.work.Done()
		m.expireSession(sess)
	})
	m.work.Add(1)
	go func() {
		defer m.work.Done()
		serveErr := server.Serve(listener)
		m.mu.Lock()
		defer m.mu.Unlock()
		if serveErr != nil && !m.closed && sess.status == sessionPending {
			sess.status = sessionFailed
			m.finishSessionLocked(sess, errors.New("OAuth callback server stopped"))
			m.cleanupSessionLocked(sess)
		}
	}()
	return SessionInfo{ID: sessionID, AuthURL: sess.authURL, ExpiresAt: sess.expiresAt}, nil
}

// Begin initiates or reuses a pending OAuth authorization session.
func (m *SessionManager) Begin(ctx context.Context) (string, time.Time, error) {
	info, err := m.BeginWithOptions(ctx, SessionOptions{})
	return info.AuthURL, info.ExpiresAt, err
}

func (m *SessionManager) expireSession(sess *loginSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == sess && sess.status == sessionPending {
		sess.status = sessionExpired
		m.finishSessionLocked(sess, ErrSessionExpired)
		m.cleanupSessionLocked(sess)
	}
}

func (m *SessionManager) failPending(sess *loginSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == sess && sess.status == sessionPending {
		sess.status = sessionFailed
		m.finishSessionLocked(sess, errors.New("Google authorization failed"))
		m.cleanupSessionLocked(sess)
	}
}

// Complete consumes an attempt exactly once, then invokes the activation hook.
func (m *SessionManager) Complete(ctx context.Context, code, state string) (UserInfo, error) {
	m.mu.Lock()
	var sid string
	if m.active != nil {
		sid = m.active.id
	}
	m.mu.Unlock()
	user, _, err := m.CompleteWithSessionID(ctx, sid, code, state)
	return user, err
}

// CompleteWithSessionID consumes an attempt with session ID and state, then invokes the activation hook.
func (m *SessionManager) CompleteWithSessionID(ctx context.Context, sessionID, code, state string) (user UserInfo, result AccountConfigResult, err error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return UserInfo{}, AccountConfigResult{}, ErrSessionManagerClosed
	}
	var sess *loginSession
	if sessionID != "" && m.sessions != nil {
		sess = m.sessions[sessionID]
	}
	if sess == nil && state != "" && m.sessions != nil {
		for _, s := range m.sessions {
			if s.state == state {
				sess = s
				break
			}
		}
	}
	if sess == nil {
		sess = m.active
	}
	if sess == nil || (sessionID != "" && sess.id != sessionID) || sess.state != state {
		m.mu.Unlock()
		return UserInfo{}, AccountConfigResult{}, ErrInvalidState
	}
	if sess.status == sessionExchanging {
		m.mu.Unlock()
		return UserInfo{}, AccountConfigResult{}, ErrLoginInProgress
	}
	if sess.status == sessionExpired {
		m.mu.Unlock()
		return UserInfo{}, AccountConfigResult{}, ErrSessionExpired
	}
	if sess.status != sessionPending {
		m.mu.Unlock()
		return UserInfo{}, AccountConfigResult{}, ErrSessionAlreadyUsed
	}
	if !time.Now().Before(sess.expiresAt) {
		sess.status = sessionExpired
		m.finishSessionLocked(sess, ErrSessionExpired)
		m.cleanupSessionLocked(sess)
		m.mu.Unlock()
		return UserInfo{}, AccountConfigResult{}, ErrSessionExpired
	}
	sess.status = sessionExchanging
	m.stopExpiryTimerLocked(sess)
	m.work.Add(1)
	m.mu.Unlock()
	defer m.work.Done()
	defer func() {
		m.mu.Lock()
		var committed *config.CommitError
		if err != nil && !errors.As(err, &committed) {
			sess.status = sessionFailed
		} else {
			sess.status = sessionCompleted
			sess.resultUser = user
			sess.resultConfig = result
		}
		m.finishSessionLocked(sess, err)
		if m.active == sess {
			m.active = nil
		}
		m.cleanupSessionLocked(sess)
		m.mu.Unlock()
	}()
	if !m.cli {
		if err := tokenOverrideError(); err != nil {
			return UserInfo{}, AccountConfigResult{}, err
		}
	}
	if strings.TrimSpace(code) == "" {
		return UserInfo{}, AccountConfigResult{}, errors.New("missing authorization code")
	}
	exchangeCtx, cancel := context.WithTimeout(ctx, DefaultHTTPTimeout)
	stop := context.AfterFunc(m.ctx, cancel)
	defer func() { stop(); cancel() }()
	tokens, err := exchangeOAuthCodeWithClient(exchangeCtx, m.client, code, sess.verifier, sess.redirectURI, sess.clientID, sess.clientSecret)
	if err != nil {
		return UserInfo{}, AccountConfigResult{}, err
	}
	user, err = fetchUserInfo(exchangeCtx, m.client, tokens.AccessToken)
	if err != nil {
		return UserInfo{}, AccountConfigResult{}, err
	}
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return UserInfo{}, AccountConfigResult{}, ErrSessionManagerClosed
	}
	if err := exchangeCtx.Err(); err != nil {
		return UserInfo{}, AccountConfigResult{}, err
	}
	if m.onConfigured != nil {
		result, err = m.onConfigured(exchangeCtx, sess.opts, tokens, sess.clientID, sess.clientSecret, user)
		var committed *config.CommitError
		if err != nil && !errors.As(err, &committed) {
			return UserInfo{}, AccountConfigResult{}, fmt.Errorf("activate account: %w", err)
		}
	}
	return user, result, err
}

// ConsumeSessionKey erases the issued key secret for a session so it cannot be read again.
func (m *SessionManager) ConsumeSessionKey(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sess, ok := m.sessions[sessionID]; ok {
		sess.resultConfig.KeySecret = ""
	}
}

// GetSession retrieves the current status of a login session by ID.
func (m *SessionManager) GetSession(sessionID string) (SessionStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var sess *loginSession
	if m.sessions != nil {
		sess = m.sessions[sessionID]
	}
	if sess == nil {
		if m.active != nil && m.active.id == sessionID {
			sess = m.active
		} else {
			return SessionStatus{}, false
		}
	}
	status := SessionStatus{
		ID:        sess.id,
		Status:    string(sess.status),
		AuthURL:   sess.authURL,
		ExpiresAt: sess.expiresAt,
	}
	if sess.status == sessionCompleted {
		status.AccountID = sess.resultConfig.AccountID
		status.Email = sess.resultUser.Email
		status.Name = sess.resultUser.Name
		if sess.resultConfig.KeySecret != "" {
			status.Key = &IssuedKey{
				ID:     sess.resultConfig.KeyID,
				Label:  sess.opts.IssueKey,
				Secret: sess.resultConfig.KeySecret,
			}
			// One-time retrieval: erase once read
			sess.resultConfig.KeySecret = ""
		}
	} else if sess.result != nil {
		status.Error = sess.result.Error()
	}
	return status, true
}

func (m *SessionManager) stopExpiryTimerLocked(sess *loginSession) {
	if sess.expiryTimer != nil {
		if sess.expiryTimer.Stop() {
			m.work.Done()
		}
		sess.expiryTimer = nil
	}
}

// cleanupSessionLocked releases the listening socket immediately but lets an
// executing callback deliver its complete response before graceful shutdown.
func (m *SessionManager) cleanupSessionLocked(sess *loginSession) {
	m.stopExpiryTimerLocked(sess)
	if sess.listener != nil {
		_ = sess.listener.Close()
		sess.listener = nil
	}
	if sess.server != nil {
		server := sess.server
		sess.server = nil
		m.work.Add(1)
		go func() {
			defer m.work.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), callbackShutdownTimeout)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				_ = server.Close()
			}
		}()
	}
}

func (m *SessionManager) finishSessionLocked(sess *loginSession, err error) {
	select {
	case <-sess.done:
		return
	default:
		sess.result = err
		close(sess.done)
	}
}

// Close permanently disables Begin, cancels completion, and joins all owned work.
func (m *SessionManager) Close() error {
	return m.closer.Close(func() error {
		m.mu.Lock()
		m.closed = true
		m.cancel()
		if m.active != nil {
			if m.active.status == sessionPending {
				m.finishSessionLocked(m.active, ErrSessionManagerClosed)
			}
			m.cleanupSessionLocked(m.active)
		}
		m.mu.Unlock()
		// Complete registers work under mu before passing the closed check.
		// Joining it also joins activation hooks, without a second lock barrier.
		m.work.Wait()
		return nil
	})
}
