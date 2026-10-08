package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"antigravity-proxy/internal/config"
)

const oauthUserInfoURL = "https://www.googleapis.com/oauth2/v2/userinfo"
const sessionLifetime = 5 * time.Minute

var (
	ErrInvalidState         = errors.New("invalid or expired OAuth state")
	ErrLoginInProgress      = errors.New("login completion already in progress")
	ErrSessionAlreadyUsed   = errors.New("OAuth session already used")
	ErrSessionExpired       = errors.New("OAuth session expired")
	ErrSessionManagerClosed = errors.New("OAuth session manager closed")
	ErrTokenEnvironment     = errors.New("remove token environment overrides to use /config/login")
)

// UserInfo contains Google account details returned by userinfo.
type UserInfo struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	Name          string `json:"name"`
	VerifiedEmail bool   `json:"verified_email"`
}

// FetchUserInfo retrieves and validates the user's identity from Google.
func FetchUserInfo(ctx context.Context, accessToken string) (UserInfo, error) {
	return FetchUserInfoWithClient(ctx, accessToken, &http.Client{Timeout: 15 * time.Second})
}

// FetchUserInfoWithClient retrieves a validated identity using the supplied client.
func FetchUserInfoWithClient(ctx context.Context, accessToken string, client *http.Client) (UserInfo, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
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

type SessionStatus string

const (
	SessionStatusPending    SessionStatus = "pending"
	SessionStatusExchanging SessionStatus = "exchanging"
	SessionStatusCompleted  SessionStatus = "completed"
	SessionStatusFailed     SessionStatus = "failed"
	SessionStatusExpired    SessionStatus = "expired"
)

// LoginSession represents one OAuth attempt. Terminal attempts cannot be replayed.
type LoginSession struct {
	State        string
	Verifier     string
	RedirectURI  string
	ClientID     string
	ClientSecret string
	AuthURL      string
	ExpiresAt    time.Time

	status      SessionStatus
	listener    net.Listener
	server      *http.Server
	expiryTimer *time.Timer
}

// OnAccountConfiguredFunc prepares, persists, and activates an authenticated account.
// It must honor ctx before committing. Close cancels ctx and joins this hook.
type OnAccountConfiguredFunc func(ctx context.Context, tokens Tokens, clientID, clientSecret string, user UserInfo) error

// SessionManager owns authorization resources, never configuration persistence.
type SessionManager struct {
	mu           sync.Mutex
	activationMu sync.Mutex
	active       *LoginSession
	onConfigured OnAccountConfiguredFunc
	client       *http.Client
	ctx          context.Context
	cancel       context.CancelFunc
	closed       bool
	closeDone    chan struct{}
	work         sync.WaitGroup
}

func NewSessionManager(onConfigured OnAccountConfiguredFunc) *SessionManager {
	return NewSessionManagerWithClient(&http.Client{Timeout: 30 * time.Second}, onConfigured)
}

// NewSessionManagerWithClient uses client for both token exchange and userinfo.
func NewSessionManagerWithClient(client *http.Client, onConfigured OnAccountConfiguredFunc) *SessionManager {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &SessionManager{client: client, onConfigured: onConfigured, ctx: ctx, cancel: cancel, closeDone: make(chan struct{})}
}

func tokenOverrideError() error {
	_, accessSet := os.LookupEnv("ANTIGRAVITY_ACCESS_TOKEN")
	_, refreshSet := os.LookupEnv("ANTIGRAVITY_REFRESH_TOKEN")
	if accessSet || refreshSet {
		return ErrTokenEnvironment
	}
	return nil
}

// Begin initiates or reuses a pending OAuth authorization session.
func (m *SessionManager) Begin(ctx context.Context) (string, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", time.Time{}, ErrSessionManagerClosed
	}
	if err := ctx.Err(); err != nil {
		return "", time.Time{}, err
	}
	if err := tokenOverrideError(); err != nil {
		return "", time.Time{}, err
	}
	now := time.Now()
	if m.active != nil {
		if m.active.status == SessionStatusExchanging {
			return "", time.Time{}, ErrLoginInProgress
		}
		if m.active.status == SessionStatusPending && now.Before(m.active.ExpiresAt) {
			return m.active.AuthURL, m.active.ExpiresAt, nil
		}
		m.cleanupSessionLocked(m.active)
	}
	clientID, clientSecret, err := resolveLoginCredentials(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	listener, callbackPort, err := listenOAuthCallbackPort()
	if err != nil {
		return "", time.Time{}, err
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/oauth-callback", callbackPort)
	authReq, err := buildAuthorizationRequest(clientID, redirectURI)
	if err != nil {
		_ = listener.Close()
		return "", time.Time{}, err
	}
	sess := &LoginSession{
		State: authReq.state, Verifier: authReq.verifier, RedirectURI: redirectURI,
		ClientID: clientID, ClientSecret: clientSecret, AuthURL: authReq.authURL,
		ExpiresAt: now.Add(sessionLifetime), status: SessionStatusPending, listener: listener,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth-callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Query().Get("state") != sess.State {
			http.Error(w, "OAuth state mismatch", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("error") != "" {
			m.failPending(sess)
			http.Error(w, "Authorization failed. You may close this tab.", http.StatusBadRequest)
			return
		}
		user, err := m.Complete(r.Context(), r.URL.Query().Get("code"), sess.State)
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
		_ = server.Serve(listener)
	}()
	return sess.AuthURL, sess.ExpiresAt, nil
}

func (m *SessionManager) expireSession(sess *LoginSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == sess && sess.status == SessionStatusPending {
		sess.status = SessionStatusExpired
		m.cleanupSessionLocked(sess)
	}
}

func (m *SessionManager) failPending(sess *LoginSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == sess && sess.status == SessionStatusPending {
		sess.status = SessionStatusFailed
		m.cleanupSessionLocked(sess)
	}
}

// Complete consumes an attempt exactly once, then invokes the activation hook.
func (m *SessionManager) Complete(ctx context.Context, code, state string) (user UserInfo, err error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return UserInfo{}, ErrSessionManagerClosed
	}
	sess := m.active
	if sess == nil || sess.State != state {
		m.mu.Unlock()
		return UserInfo{}, ErrInvalidState
	}
	if sess.status == SessionStatusExchanging {
		m.mu.Unlock()
		return UserInfo{}, ErrLoginInProgress
	}
	if sess.status == SessionStatusExpired {
		m.mu.Unlock()
		return UserInfo{}, ErrSessionExpired
	}
	if sess.status != SessionStatusPending {
		m.mu.Unlock()
		return UserInfo{}, ErrSessionAlreadyUsed
	}
	if !time.Now().Before(sess.ExpiresAt) {
		sess.status = SessionStatusExpired
		m.cleanupSessionLocked(sess)
		m.mu.Unlock()
		return UserInfo{}, ErrSessionExpired
	}
	sess.status = SessionStatusExchanging
	m.stopExpiryTimerLocked(sess)
	m.work.Add(1)
	m.mu.Unlock()
	defer m.work.Done()
	defer func() {
		m.mu.Lock()
		var committed *config.CommitError
		if err != nil && !errors.As(err, &committed) {
			sess.status = SessionStatusFailed
		} else {
			sess.status = SessionStatusCompleted
		}
		m.cleanupSessionLocked(sess)
		m.mu.Unlock()
	}()
	if err := tokenOverrideError(); err != nil {
		return UserInfo{}, err
	}
	if strings.TrimSpace(code) == "" {
		return UserInfo{}, errors.New("missing authorization code")
	}
	exchangeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	stop := context.AfterFunc(m.ctx, cancel)
	defer func() { stop(); cancel() }()
	tokens, err := exchangeOAuthCodeWithClient(exchangeCtx, m.client, code, sess.Verifier, sess.RedirectURI, sess.ClientID, sess.ClientSecret)
	if err != nil {
		return UserInfo{}, err
	}
	user, err = fetchUserInfo(exchangeCtx, m.client, tokens.AccessToken)
	if err != nil {
		return UserInfo{}, err
	}
	m.activationMu.Lock()
	defer m.activationMu.Unlock()
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return UserInfo{}, ErrSessionManagerClosed
	}
	if err := exchangeCtx.Err(); err != nil {
		return UserInfo{}, err
	}
	if m.onConfigured != nil {
		if err := m.onConfigured(exchangeCtx, tokens, sess.ClientID, sess.ClientSecret, user); err != nil {
			return UserInfo{}, fmt.Errorf("activate account: %w", err)
		}
	}
	return user, nil
}

func (m *SessionManager) stopExpiryTimerLocked(sess *LoginSession) {
	if sess.expiryTimer != nil {
		if sess.expiryTimer.Stop() {
			m.work.Done()
		}
		sess.expiryTimer = nil
	}
}

// cleanupSessionLocked releases the listening socket immediately but lets an
// executing callback deliver its complete response before graceful shutdown.
func (m *SessionManager) cleanupSessionLocked(sess *LoginSession) {
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
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				_ = server.Close()
			}
		}()
	}
}

// Close permanently disables Begin, cancels completion, and joins all owned work.
func (m *SessionManager) Close() error {
	m.mu.Lock()
	if m.closed {
		done := m.closeDone
		m.mu.Unlock()
		<-done
		return nil
	}
	m.closed = true
	m.cancel()
	if m.active != nil {
		m.cleanupSessionLocked(m.active)
	}
	m.mu.Unlock()
	// No hook can pass the closed check after this lifecycle barrier.
	m.activationMu.Lock()
	m.activationMu.Unlock()
	m.work.Wait()
	close(m.closeDone)
	return nil
}
