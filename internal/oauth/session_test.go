package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-proxy/internal/config"
)

func testUserInfo() UserInfo {
	return UserInfo{ID: "1001", Email: "user@example.com", Name: "Test User", VerifiedEmail: true}
}

type sessionTransportFunc func(*http.Request) (*http.Response, error)

func (f sessionTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func sessionResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func sessionClient(t *testing.T, handler http.Handler) *http.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: oauthTestTransport{base: http.DefaultTransport, target: target}, Timeout: 5 * time.Second}
}

func successfulSessionClient(t *testing.T, user UserInfo) *http.Client {
	return sessionClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(Tokens{AccessToken: "new-access", RefreshToken: "new-refresh"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer new-access" {
			t.Error("profile fetch lost access token")
		}
		_ = json.NewEncoder(w).Encode(user)
	}))
}

func beginSession(t *testing.T, mgr *SessionManager) (*url.URL, string) {
	t.Helper()
	address, expiry, err := mgr.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(expiry) <= 0 || time.Until(expiry) > sessionLifetime {
		t.Fatal("invalid session lifetime")
	}
	authorization, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	query := authorization.Query()
	if query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" {
		t.Fatal("missing PKCE")
	}
	callback, err := url.Parse(query.Get("redirect_uri"))
	if err != nil {
		t.Fatal(err)
	}
	if callback.Hostname() != "127.0.0.1" {
		t.Fatal("callback not bound to loopback")
	}
	return callback, query.Get("state")
}

func requireReleasedListener(t *testing.T, callback *url.URL) {
	t.Helper()
	listener, err := net.Listen("tcp", callback.Host)
	if err != nil {
		t.Fatalf("callback listener was not released: %v", err)
	}
	_ = listener.Close()
}

func awaitSessionResult(t *testing.T, results <-chan error) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("session operation did not finish")
		return nil
	}
}

func TestFetchUserInfo(t *testing.T) {
	client := sessionClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-access" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(testUserInfo())
	}))
	info, err := FetchUserInfoWithClient(context.Background(), "test-access", client)
	if err != nil || info != testUserInfo() {
		t.Fatalf("profile = %+v, %v", info, err)
	}
	if _, err := FetchUserInfoWithClient(context.Background(), "wrong", client); err == nil {
		t.Fatal("unauthorized profile accepted")
	}
}

func TestSessionManagerLifecycle(t *testing.T) {
	clearOAuthEnvironment(t)
	var calls atomic.Int32
	mgr := NewSessionManagerWithClient(successfulSessionClient(t, testUserInfo()), func(ctx context.Context, tokens Tokens, id, secret string, user UserInfo) error {
		calls.Add(1)
		if ctx.Err() != nil || tokens.AccessToken != "new-access" || user != testUserInfo() || id == "" || secret == "" {
			t.Error("hook received wrong account")
		}
		return nil
	})
	t.Cleanup(func() { _ = mgr.Close() })
	callback, state := beginSession(t, mgr)
	first, expiry, err := mgr.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, expiry2, err := mgr.Begin(context.Background())
	if err != nil || first != second || !expiry.Equal(expiry2) {
		t.Fatal("pending Begin did not reuse attempt")
	}
	if _, err := mgr.Complete(context.Background(), "code", "wrong"); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
	user, err := mgr.Complete(context.Background(), "code", state)
	if err != nil || user != testUserInfo() || calls.Load() != 1 {
		t.Fatalf("completion = %+v, %v", user, err)
	}
	if _, err := mgr.Complete(context.Background(), "code", state); !errors.Is(err, ErrSessionAlreadyUsed) {
		t.Fatal("completed attempt replayed", err)
	}
	requireReleasedListener(t, callback)
}

func TestSessionCallbackDeliversFullPlainTextProfile(t *testing.T) {
	clearOAuthEnvironment(t)
	user := testUserInfo()
	user.Name = "<script>alert('profile')</script> & \"quoted\""
	mgr := NewSessionManagerWithClient(successfulSessionClient(t, user), nil)
	t.Cleanup(func() { _ = mgr.Close() })
	callback, state := beginSession(t, mgr)
	callback.RawQuery = url.Values{"state": {state}, "code": {"code"}}.Encode()
	response, err := (&http.Client{Timeout: 5 * time.Second}).Get(callback.String())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	want := "Authorization complete. Logged in as " + user.Name + " (" + user.Email + "). You may close this tab and return to the proxy.\n"
	if err != nil || response.StatusCode != http.StatusOK || string(body) != want {
		t.Fatalf("callback delivery = %q, %v", body, err)
	}
	if response.Header.Get("Content-Type") != "text/plain; charset=utf-8" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("profile markup was not served as non-cacheable text")
	}
	requireReleasedListener(t, callback)
}

func TestSessionFailuresAreTerminalAndReleaseListener(t *testing.T) {
	for _, scenario := range []struct {
		name, token, profile string
		status               int
	}{
		{"token rejection", "", "", http.StatusBadRequest},
		{"missing token", `{}`, `{"id":"1001","email":"user@example.com"}`, http.StatusOK},
		{"empty ID", `{"access_token":"new-access"}`, `{"email":"user@example.com"}`, http.StatusOK},
		{"blank ID", `{"access_token":"new-access"}`, `{"id":" ","email":"user@example.com"}`, http.StatusOK},
		{"empty email", `{"access_token":"new-access"}`, `{"id":"1001"}`, http.StatusOK},
		{"malformed email", `{"access_token":"new-access"}`, `{"id":"1001","email":"not-an-email"}`, http.StatusOK},
		{"malformed identity", `{"access_token":"new-access"}`, `{"id":1001,"email":"user@example.com"}`, http.StatusOK},
		{"malformed profile JSON", `{"access_token":"new-access"}`, `{"id":`, http.StatusOK},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			clearOAuthEnvironment(t)
			var activated atomic.Int32
			client := sessionClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					w.WriteHeader(scenario.status)
					_, _ = io.WriteString(w, scenario.token)
					return
				}
				_, _ = io.WriteString(w, scenario.profile)
			}))
			mgr := NewSessionManagerWithClient(client, func(context.Context, Tokens, string, string, UserInfo) error { activated.Add(1); return nil })
			t.Cleanup(func() { _ = mgr.Close() })
			callback, state := beginSession(t, mgr)
			if _, err := mgr.Complete(context.Background(), "code", state); err == nil {
				t.Fatal("invalid account committed")
			}
			if activated.Load() != 0 {
				t.Fatal("invalid identity activated")
			}
			if _, err := mgr.Complete(context.Background(), "code", state); !errors.Is(err, ErrSessionAlreadyUsed) {
				t.Fatal("failed attempt could be retried", err)
			}
			requireReleasedListener(t, callback)
		})
	}
}

func TestSessionExpiryAndAuthorizationErrorReleaseListener(t *testing.T) {
	for _, scenario := range []string{"timer", "expired completion", "authorization error", "missing code"} {
		t.Run(scenario, func(t *testing.T) {
			clearOAuthEnvironment(t)
			mgr := NewSessionManager(nil)
			t.Cleanup(func() { _ = mgr.Close() })
			callback, state := beginSession(t, mgr)
			switch scenario {
			case "timer":
				mgr.mu.Lock()
				mgr.active.expiryTimer.Reset(time.Millisecond)
				mgr.mu.Unlock()
				deadline := time.Now().Add(5 * time.Second)
				for {
					mgr.mu.Lock()
					expired := mgr.active.status == SessionStatusExpired
					mgr.mu.Unlock()
					if expired {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("pending timer did not expire attempt")
					}
					time.Sleep(time.Millisecond)
				}
			case "expired completion":
				mgr.mu.Lock()
				mgr.active.ExpiresAt = time.Now().Add(-time.Second)
				mgr.mu.Unlock()
				if _, err := mgr.Complete(context.Background(), "code", state); !errors.Is(err, ErrSessionExpired) {
					t.Fatal(err)
				}
			default:
				values := url.Values{"state": {state}}
				if scenario == "authorization error" {
					values.Set("error", "<secret>")
				}
				callback.RawQuery = values.Encode()
				response, err := (&http.Client{Timeout: 5 * time.Second}).Get(callback.String())
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil || response.StatusCode != http.StatusBadRequest || strings.Contains(string(body), "<secret>") {
					t.Fatal("callback error leaked or failed delivery", err)
				}
				if _, err := mgr.Complete(context.Background(), "code", state); !errors.Is(err, ErrSessionAlreadyUsed) {
					t.Fatal("callback error did not consume attempt", err)
				}
			}
			requireReleasedListener(t, callback)
		})
	}
}

func TestConcurrentCompletionExchangesAtMostOnce(t *testing.T) {
	clearOAuthEnvironment(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var exchanges, activations atomic.Int32
	client := &http.Client{Transport: sessionTransportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			exchanges.Add(1)
			close(entered)
			select {
			case <-release:
				return sessionResponse(`{"access_token":"new-access"}`), nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return sessionResponse(`{"id":"1001","email":"user@example.com"}`), nil
	})}
	mgr := NewSessionManagerWithClient(client, func(context.Context, Tokens, string, string, UserInfo) error { activations.Add(1); return nil })
	t.Cleanup(func() { _ = mgr.Close() })
	_, state := beginSession(t, mgr)
	results := make(chan error, 1)
	go func() { _, err := mgr.Complete(context.Background(), "first", state); results <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("exchange did not start")
	}
	for range 16 {
		if _, err := mgr.Complete(context.Background(), "second", state); !errors.Is(err, ErrLoginInProgress) {
			t.Fatal("concurrent completion accepted", err)
		}
	}
	close(release)
	if err := awaitSessionResult(t, results); err != nil {
		t.Fatal(err)
	}
	if exchanges.Load() != 1 || activations.Load() != 1 {
		t.Fatal("more than one exchange or activation")
	}
}

func TestCloseCancelsInflightAndPreventsLaterActivation(t *testing.T) {
	clearOAuthEnvironment(t)
	entered := make(chan struct{})
	var activations atomic.Int32
	client := &http.Client{Transport: sessionTransportFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	mgr := NewSessionManagerWithClient(client, func(context.Context, Tokens, string, string, UserInfo) error { activations.Add(1); return nil })
	t.Cleanup(func() { _ = mgr.Close() })
	callback, state := beginSession(t, mgr)
	results := make(chan error, 1)
	go func() { _, err := mgr.Complete(context.Background(), "code", state); results <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("exchange did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- mgr.Close() }()
	if err := awaitSessionResult(t, closed); err != nil {
		t.Fatal(err)
	}
	if err := awaitSessionResult(t, results); err == nil {
		t.Fatal("canceled exchange succeeded")
	}
	if activations.Load() != 0 {
		t.Fatal("shutdown activated account")
	}
	if _, _, err := mgr.Begin(context.Background()); !errors.Is(err, ErrSessionManagerClosed) {
		t.Fatal("closed manager restarted", err)
	}
	if _, err := mgr.Complete(context.Background(), "code", state); !errors.Is(err, ErrSessionManagerClosed) {
		t.Fatal("closed manager completed", err)
	}
	requireReleasedListener(t, callback)
	if err := mgr.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCloseJoinsActivationHook(t *testing.T) {
	clearOAuthEnvironment(t)
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	mgr := NewSessionManagerWithClient(successfulSessionClient(t, testUserInfo()), func(ctx context.Context, _ Tokens, _, _ string, _ UserInfo) error {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		return ctx.Err()
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		_ = mgr.Close()
	})
	_, state := beginSession(t, mgr)
	results := make(chan error, 1)
	go func() { _, err := mgr.Complete(context.Background(), "code", state); results <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("hook did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- mgr.Close() }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("hook context not canceled")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before activation hook exited")
	default:
	}
	close(release)
	if err := awaitSessionResult(t, results); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := awaitSessionResult(t, closed); err != nil {
		t.Fatal(err)
	}
}

func TestCommittedPersistenceWarningIsPreservedAndConsumed(t *testing.T) {
	clearOAuthEnvironment(t)
	warning := &config.CommitError{Err: errors.New("directory sync failed")}
	mgr := NewSessionManagerWithClient(successfulSessionClient(t, testUserInfo()), func(context.Context, Tokens, string, string, UserInfo) error { return warning })
	t.Cleanup(func() { _ = mgr.Close() })
	callback, state := beginSession(t, mgr)
	_, err := mgr.Complete(context.Background(), "code", state)
	var committed *config.CommitError
	if !errors.As(err, &committed) || committed != warning {
		t.Fatal("committed persistence warning lost", err)
	}
	if _, err := mgr.Complete(context.Background(), "code", state); !errors.Is(err, ErrSessionAlreadyUsed) {
		t.Fatal("committed warning allowed replay", err)
	}
	requireReleasedListener(t, callback)
}

func TestSessionManagerEnvOverrideRejection(t *testing.T) {
	clearOAuthEnvironment(t)
	mgr := NewSessionManager(nil)
	t.Cleanup(func() { _ = mgr.Close() })
	for _, name := range []string{"ANTIGRAVITY_ACCESS_TOKEN", "ANTIGRAVITY_REFRESH_TOKEN"} {
		t.Setenv(name, "override")
		if _, _, err := mgr.Begin(context.Background()); err == nil {
			t.Fatal("environment token override accepted")
		}
		t.Setenv(name, "")
	}
}

func TestSessionManagerRejectsExplicitEmptyTokenOverrides(t *testing.T) {
	for _, name := range []string{"ANTIGRAVITY_ACCESS_TOKEN", "ANTIGRAVITY_REFRESH_TOKEN"} {
		t.Run(name, func(t *testing.T) {
			clearOAuthEnvironment(t)
			mgr := NewSessionManagerWithClient(successfulSessionClient(t, testUserInfo()), nil)
			t.Cleanup(func() { _ = mgr.Close() })
			callback, state := beginSession(t, mgr)
			t.Setenv(name, "")
			if _, err := mgr.Complete(context.Background(), "code", state); !errors.Is(err, ErrTokenEnvironment) {
				t.Fatalf("empty override shadowed saved credentials: %v", err)
			}
			requireReleasedListener(t, callback)
			if _, _, err := mgr.Begin(context.Background()); !errors.Is(err, ErrTokenEnvironment) {
				t.Fatalf("new attempt accepted empty token override: %v", err)
			}
		})
	}
}

func TestCallbackRejectsStraysWithoutConsumingAttempt(t *testing.T) {
	clearOAuthEnvironment(t)
	mgr := NewSessionManagerWithClient(successfulSessionClient(t, testUserInfo()), nil)
	t.Cleanup(func() { _ = mgr.Close() })
	callback, state := beginSession(t, mgr)
	client := &http.Client{Timeout: 5 * time.Second}
	for _, stray := range []struct {
		method, state string
		status        int
	}{
		{http.MethodGet, "wrong-state", http.StatusBadRequest},
		{http.MethodPost, state, http.StatusMethodNotAllowed},
	} {
		callback.RawQuery = url.Values{"state": {stray.state}, "code": {"code"}}.Encode()
		request, err := http.NewRequest(stray.method, callback.String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != stray.status || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("stray callback response", err)
		}
	}
	if _, err := mgr.Complete(context.Background(), "code", state); err != nil {
		t.Fatal("stray callback consumed attempt", err)
	}
}

func TestCLIPersistenceRejectsMissingIdentity(t *testing.T) {
	for _, user := range []UserInfo{{}, {ID: "1001"}, {Email: "user@example.com"}, {ID: "1001", Email: "malformed"}} {
		if err := saveLoginTokens("", Tokens{AccessToken: "new-access"}, "id", "secret", user); err == nil {
			t.Fatal("CLI persistence accepted missing identity")
		}
	}
}

func TestCallbackReportsCommittedWarningWithoutLeakingCause(t *testing.T) {
	clearOAuthEnvironment(t)
	mgr := NewSessionManagerWithClient(successfulSessionClient(t, testUserInfo()), func(context.Context, Tokens, string, string, UserInfo) error {
		return &config.CommitError{Err: errors.New("private-filesystem-cause")}
	})
	t.Cleanup(func() { _ = mgr.Close() })
	callback, state := beginSession(t, mgr)
	callback.RawQuery = url.Values{"state": {state}, "code": {"code"}}.Encode()
	response, err := (&http.Client{Timeout: 5 * time.Second}).Get(callback.String())
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusInternalServerError || !strings.Contains(string(body), "saved and activated") || strings.Contains(string(body), "private-filesystem-cause") {
		t.Fatal("committed callback warning was lost or leaked details", err)
	}
	if _, err := mgr.Complete(context.Background(), "code", state); !errors.Is(err, ErrSessionAlreadyUsed) {
		t.Fatal("committed callback replayed", err)
	}
	requireReleasedListener(t, callback)
}
