package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-proxy/internal/config"
)

func TestUpstreamFallbackStatusPolicy(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var dailyCalls, prodCalls atomic.Int32
			daily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				dailyCalls.Add(1)
				w.Header().Set("Retry-After", "37")
				w.Header().Set("Set-Cookie", "secret=token")
				w.Header().Set("X-Secret", "token")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "credential-secret")
			}))
			defer daily.Close()
			prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				prodCalls.Add(1)
				_, _ = io.WriteString(w, "{}")
			}))
			defer prod.Close()
			p := New(config.Config{DailyEndpoint: daily.URL, ProdEndpoint: prod.URL})
			resp, err := p.postToAntigravity(context.Background(), "token", "/operation", "application/json", map[string]string{})
			fallback := status == 404 || status >= 500
			if fallback {
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if prodCalls.Load() != 1 {
					t.Fatal("retryable status did not fall back")
				}
			} else {
				var failure *upstreamError
				if !errors.As(err, &failure) || failure.Status != status || failure.Headers.Get("Retry-After") != "37" {
					t.Fatalf("status and backoff lost: %v", err)
				}
				if prodCalls.Load() != 0 {
					t.Fatal("terminal status fell back")
				}
				if strings.Contains(err.Error(), "credential-secret") || len(failure.Headers) != 1 {
					t.Fatal("unsafe provider content retained")
				}
			}
			if dailyCalls.Load() != 1 {
				t.Fatal("daily endpoint not first")
			}
		})
	}
}

func TestUpstreamTransportFailureFallbackAndSecrecy(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := closed.URL
	closed.Close()
	var calls atomic.Int32
	prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, "{}") }))
	defer prod.Close()
	p := New(config.Config{DailyEndpoint: endpoint, ProdEndpoint: prod.URL})
	resp, err := p.postToAntigravity(context.Background(), "credential-secret", "/operation?secret=query", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls.Load() != 1 {
		t.Fatal("transport failure did not fall back")
	}
	p.cfg.ProdEndpoint = endpoint
	_, err = p.postToAntigravity(context.Background(), "credential-secret", "/operation?secret=query", "application/json", nil)
	if err == nil || strings.Contains(err.Error(), endpoint) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe transport error: %v", err)
	}
}

func TestSafeUpstreamHeaders(t *testing.T) {
	for _, value := range []string{"-1", "credential-secret", "1\r\nX-Secret: leak"} {
		if safeUpstreamHeaders(http.Header{"Retry-After": {value}}).Get("Retry-After") != "" {
			t.Fatal("unsafe retry header accepted")
		}
	}
	date := time.Now().UTC().Truncate(time.Second).Add(time.Minute).Format(http.TimeFormat)
	h := safeUpstreamHeaders(http.Header{"Retry-After": {date}, "Content-Language": {"en-US"}, "Cache-Control": {"no-store"}, "Authorization": {"secret"}})
	if h.Get("Retry-After") != date || h.Get("Content-Language") != "" || h.Get("Cache-Control") != "no-store" || len(h) != 2 {
		t.Fatalf("safe headers = %v", h)
	}
}

func TestProjectDiscoverySharedAndWaiterCancellation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(started)
		<-release
		io.WriteString(w, `{"cloudaicompanionProject":"discovered"}`)
	}))
	defer upstream.Close()
	p := New(config.Config{DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
	owner := make(chan error, 1)
	go func() {
		id, err := p.getProjectID(context.Background(), "token")
		if id != "discovered" && err == nil {
			err = errors.New("wrong project")
		}
		owner <- err
	}()
	<-started
	if !p.projectMu.TryLock() {
		close(release)
		t.Fatal("network discovery held projectMu")
	}
	p.projectMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() { _, err := p.getProjectID(ctx, "token"); waiter <- err }()
	cancel()
	select {
	case err := <-waiter:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("waiter did not cancel")
	}
	const waiters = 12
	var wg sync.WaitGroup
	wg.Add(waiters)
	for range waiters {
		go func() {
			defer wg.Done()
			id, err := p.getProjectID(context.Background(), "token")
			if err != nil || id != "discovered" {
				t.Errorf("shared discovery = %q, %v", id, err)
			}
		}()
	}
	close(release)
	if err := <-owner; err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("discovery requests = %d", calls.Load())
	}
}

func TestProjectDiscoveryFailureNeverCachesFallback(t *testing.T) {
	for _, body := range []string{`{"done":true}`, `{"done":true,"response":{"cloudaicompanionProject":""}}`, `not-json`} {
		t.Run(body, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1internal:loadCodeAssist" {
					io.WriteString(w, `{}`)
					return
				}
				io.WriteString(w, body)
			}))
			defer upstream.Close()
			p := New(config.Config{DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
			id, err := p.getProjectID(context.Background(), "token")
			if err == nil || id != "" || p.projectID != "" {
				t.Fatalf("failed discovery cached %q: %v", id, err)
			}
		})
	}
}

func TestOnboardingFailureExhaustionAndCancellation(t *testing.T) {
	for _, status := range []int{400, 429, 503, 200} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
				io.WriteString(w, `{"done":false}`)
			}))
			defer upstream.Close()
			p := New(config.Config{DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
			id, err := p.pollOnboarding(context.Background(), "token", nil, 2, 0)
			if err == nil || id != "" {
				t.Fatal("onboarding failure lost")
			}
			want := int32(1)
			if status == 200 {
				want = 2
				if !strings.Contains(err.Error(), "polling limit") {
					t.Fatal(err)
				}
			}
			if calls.Load() != want {
				t.Fatalf("onboard calls = %d", calls.Load())
			}
		})
	}
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); io.WriteString(w, `{"done":false}`) }))
	defer upstream.Close()
	p := New(config.Config{DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := p.pollOnboarding(ctx, "token", nil, 10, time.Hour); result <- err }()
	<-started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("onboarding polling did not cancel")
	}
}

func TestProjectRejectionOnlyInvalidatesAfterTerminalScopedFailure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		dailyStatus int
		dailyBody   string
		prodStatus  int
		prodBody    string
		rediscover  bool
	}{
		{"daily missing production succeeds", 404, projectErrorBody("project"), 200, "{}", false},
		{"generic daily missing production succeeds", 404, "{}", 200, "{}", false},
		{"generic forbidden", 403, "credential-secret", 200, "{}", false},
		{"generic missing", 404, "{}", 404, "{}", false},
		{"generic permission detail", 403, `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"googleapis.com","reason":"PERMISSION_DENIED","metadata":{"project":"project"}}]}}`, 200, "{}", false},
		{"unrelated project", 403, projectErrorBody("other-project"), 200, "{}", false},
		{"untrusted detail domain", 403, strings.ReplaceAll(projectErrorBody("project"), "googleapis.com", "untrusted.invalid"), 200, "{}", false},
		{"oversized structured error", 403, strings.ReplaceAll(projectErrorBody("project"), "credential-secret", strings.Repeat("x", 17<<10)), 200, "{}", false},
		{"permission resource detail", 403, `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ResourceInfo","resourceType":"cloudresourcemanager.googleapis.com/Project","resourceName":"projects/project"}]}}`, 200, "{}", false},
		{"terminal forbidden project", 403, projectErrorBody("project"), 200, "{}", true},
		{"terminal missing project", 404, projectErrorBody("project"), 404, projectErrorBody("project"), true},
		{"only earlier failure scoped", 404, projectErrorBody("project"), 404, "{}", false},
		{"typed project missing", 404, `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ResourceInfo","resourceType":"cloudresourcemanager.googleapis.com/Project","resourceName":"projects/project"}]}}`, 404, `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ResourceInfo","resourceType":"cloudresourcemanager.googleapis.com/Project","resourceName":"projects/project"}]}}`, true},
	} {
		for _, configured := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/discovered", true: "/configured"}[configured], func(t *testing.T) {
				var discoveries atomic.Int32
				daily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v1internal:loadCodeAssist" {
						discoveries.Add(1)
						io.WriteString(w, `{"cloudaicompanionProject":"project"}`)
						return
					}
					w.WriteHeader(tc.dailyStatus)
					io.WriteString(w, tc.dailyBody)
				}))
				defer daily.Close()
				prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.prodStatus)
					io.WriteString(w, tc.prodBody)
				}))
				defer prod.Close()
				cfg := config.Config{DailyEndpoint: daily.URL, ProdEndpoint: prod.URL}
				if configured {
					cfg.ProjectID = "project"
				}
				p := New(cfg)
				if id, err := p.getProjectID(context.Background(), "token"); err != nil || id != "project" {
					t.Fatalf("initial discovery = %q, %v", id, err)
				}
				response, err := p.postToAntigravity(context.Background(), "token", "/operation", "application/json", map[string]string{"project": "project"})
				if response != nil {
					response.Body.Close()
				}
				if err != nil && strings.Contains(err.Error(), "credential-secret") {
					t.Fatal("provider body leaked")
				}
				wantSuccess := tc.dailyStatus == 404 && tc.prodStatus == 200
				if (err == nil) != wantSuccess {
					t.Fatalf("request error = %v, want success=%v", err, wantSuccess)
				}
				if id, err := p.getProjectID(context.Background(), "token"); err != nil || id != "project" {
					t.Fatalf("next project = %q, %v", id, err)
				}
				want := int32(1)
				if tc.rediscover {
					want++
				}
				if configured {
					want = 0
				}
				if discoveries.Load() != want {
					t.Fatalf("discovery count = %d, want %d", discoveries.Load(), want)
				}
			})
		}
	}
}

func projectErrorBody(project string) string {
	return `{"error":{"message":"credential-secret","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"PROJECT_NOT_FOUND","domain":"googleapis.com","metadata":{"project":"` + project + `"}}]}}`
}

func TestProjectDiscoveryUsesDailyAndRetryPolicy(t *testing.T) {
	for _, status := range []int{400, 429, 404, 503} {
		var prodCalls atomic.Int32
		daily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			prodCalls.Add(1)
			io.WriteString(w, `{"cloudaicompanionProject":"prod-project"}`)
		}))
		p := New(config.Config{DailyEndpoint: daily.URL, ProdEndpoint: prod.URL})
		id, err := p.getProjectID(context.Background(), "token")
		if status == 404 || status == 503 {
			if err != nil || id != "prod-project" || prodCalls.Load() != 1 {
				t.Fatalf("retry discovery = %q, %v", id, err)
			}
		} else if err == nil || prodCalls.Load() != 0 {
			t.Fatal("discovery retried terminal daily response")
		}
		daily.Close()
		prod.Close()
	}
}

func waitForProjectWaiters(t *testing.T, p *Proxy, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		p.projectMu.Lock()
		joined := p.projectFlight != nil && p.projectFlight.waiters == count
		p.projectMu.Unlock()
		if joined {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("discovery callers did not join shared request")
}

func TestDiscoveryInitiatorCancellationDoesNotAbortOtherWaiters(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			io.WriteString(w, `{"cloudaicompanionProject":"shared"}`)
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	p := New(config.Config{DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner, waiter := make(chan error, 1), make(chan error, 1)
	go func() { _, err := p.getProjectID(ctx, "token"); owner <- err }()
	<-started
	go func() {
		id, err := p.getProjectID(context.Background(), "token")
		if err == nil && id != "shared" {
			err = errors.New("wrong project")
		}
		waiter <- err
	}()
	waitForProjectWaiters(t, p, 2)
	cancel()
	if err := <-owner; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	if err := <-waiter; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("initiator cancellation restarted shared discovery")
	}
}

func TestDiscoveryCancelsNetworkWhenAllWaitersLeave(t *testing.T) {
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// net/http starts monitoring peer disconnects only after the POST body
		// is consumed. An unread body would make this fixture miss cancellation.
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("consume discovery body: %v", err)
			return
		}
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	defer func() {
		close(release)
		upstream.CloseClientConnections()
		upstream.Close()
	}()
	p := New(config.Config{DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := p.getProjectID(ctx, "token"); result <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("discovery request did not reach server")
	}
	p.projectMu.Lock()
	flight := p.projectFlight
	p.projectMu.Unlock()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("discovery waiter did not return cancellation")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("unobserved discovery kept network request alive")
	}
	select {
	case <-flight.done:
	case <-time.After(time.Second):
		t.Fatal("canceled discovery did not finish")
	}
	if !errors.Is(flight.err, context.Canceled) {
		t.Fatalf("shared network result = %v", flight.err)
	}
	p.projectMu.Lock()
	retained := p.projectFlight != nil || p.projectID != ""
	p.projectMu.Unlock()
	if retained {
		t.Fatal("abandoned discovery retained flight or cached a project")
	}
}

func TestStaleProjectFailureCannotClearRediscoveredSameID(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var slow atomic.Bool
	var discoveries atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1internal:loadCodeAssist" {
			discoveries.Add(1)
			io.WriteString(w, `{"cloudaicompanionProject":"same-project"}`)
			return
		}
		if slow.CompareAndSwap(false, true) {
			close(started)
			<-release
		}
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, projectErrorBody("same-project"))
	}))
	defer upstream.Close()
	p := New(config.Config{DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
	if _, err := p.getProjectID(context.Background(), "token"); err != nil {
		t.Fatal(err)
	}
	oldRequest := make(chan error, 1)
	go func() {
		_, err := p.postToAntigravity(context.Background(), "token", "/operation", "application/json", map[string]string{"project": "same-project"})
		oldRequest <- err
	}()
	<-started
	if _, err := p.postToAntigravity(context.Background(), "token", "/operation", "application/json", map[string]string{"project": "same-project"}); err == nil {
		t.Fatal("project rejection lost")
	}
	if _, err := p.getProjectID(context.Background(), "token"); err != nil {
		t.Fatal(err)
	}
	if discoveries.Load() != 2 {
		close(release)
		t.Fatal("terminal project rejection did not trigger rediscovery")
	}
	close(release)
	if err := <-oldRequest; err == nil {
		t.Fatal("delayed rejection lost")
	}
	if p.projectID != "same-project" {
		t.Fatal("old response cleared newly rediscovered project")
	}
}

type upstreamLogBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *upstreamLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *upstreamLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func TestUpstreamLogsAndOAuthErrorsExcludeSecrets(t *testing.T) {
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", "")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", "")
	var logs upstreamLogBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, "body-secret")
	}))
	defer upstream.Close()
	p := New(config.Config{DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
	_, err := p.postToAntigravity(context.Background(), "token-secret", "/operation?query-secret=value", "application/json", nil)
	if err == nil {
		t.Fatal("missing provider failure")
	}
	p.cfg.RefreshToken = "refresh-secret"
	p.cfg.OAuthClientID = "client-secret"
	_, oauthErr := p.accessToken(context.Background())
	if oauthErr == nil {
		t.Fatal("partial OAuth credentials unexpectedly succeeded")
	}
	for _, secret := range []string{"token-secret", "body-secret", "query-secret", "refresh-secret", "client-secret", upstream.URL} {
		if strings.Contains(logs.String(), secret) || strings.Contains(err.Error(), secret) || strings.Contains(oauthErr.Error(), secret) {
			t.Fatalf("secret leaked: %q", secret)
		}
	}
	if !strings.Contains(logs.String(), `"endpoint":"daily"`) || !strings.Contains(logs.String(), `"status":400`) {
		t.Fatal("structured upstream failure metadata missing")
	}
}
