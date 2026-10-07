package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"antigravity-proxy/internal/config"
)

func TestResponseActivityHasNoTotalDeadline(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for range 6 {
			if _, err := io.WriteString(w, "x"); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}))
	defer upstream.Close()
	p := New(config.Config{DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
	p.client = newUpstreamClient(200*time.Millisecond, 400*time.Millisecond)
	p.rpcTimeout = 100 * time.Millisecond
	defer p.client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response, err := p.postToAntigravity(ctx, "token", "/v1internal:streamGenerateContent", "text/event-stream", generationEnvelope{Request: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "xxxxxx" {
		t.Fatalf("active response ended early: %q, %v", body, err)
	}
}

func TestResponseBodyIdleDeadlineCancelsNetworkRead(t *testing.T) {
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	client := newUpstreamClient(time.Second, 100*time.Millisecond)
	defer client.CloseIdleConnections()
	response, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	result := make(chan error, 1)
	go func() { _, err := io.ReadAll(response.Body); result <- err }()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("idle read error = %v", err)
		}
	case <-time.After(2 * time.Second):
		response.Body.Close()
		t.Fatal("idle response did not expire")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("idle timeout did not cancel network request")
	}
}

func TestResponseBodyCancellationAndClose(t *testing.T) {
	for _, closeBody := range []bool{false, true} {
		t.Run(map[bool]string{false: "context", true: "close"}[closeBody], func(t *testing.T) {
			canceled := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(canceled)
			}))
			defer upstream.Close()
			p := New(config.Config{DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
			p.client = newUpstreamClient(time.Second, time.Hour)
			defer p.client.CloseIdleConnections()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			response, err := p.postToAntigravity(ctx, "token", "/operation", "application/json", map[string]string{})
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			result := make(chan error, 1)
			go func() { _, err := io.ReadAll(response.Body); result <- err }()
			if closeBody {
				response.Body.Close()
			} else {
				cancel()
			}
			select {
			case err := <-result:
				if err == nil || !closeBody && !errors.Is(err, context.Canceled) {
					t.Fatalf("body termination error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("body termination did not interrupt read")
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("network cancellation missing")
			}
		})
	}
}

func TestUpstreamHeaderDeadline(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer upstream.Close()
	client := newUpstreamClient(50*time.Millisecond, time.Hour)
	defer client.CloseIdleConnections()
	started := time.Now()
	_, err := client.Get(upstream.URL)
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("header deadline not enforced: %v", err)
	}
}

func TestIdleResponseTransportPreservesConnectionCleanup(t *testing.T) {
	idle, closed := make(chan struct{}, 1), make(chan struct{}, 1)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "complete") }))
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateIdle:
			select {
			case idle <- struct{}{}:
			default:
			}
		case http.StateClosed:
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	upstream.Start()
	defer upstream.Close()
	client := newUpstreamClient(time.Second, time.Second)
	defer client.CloseIdleConnections()
	response, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	select {
	case <-idle:
	case <-time.After(time.Second):
		t.Fatal("completed connection did not become idle")
	}
	client.CloseIdleConnections()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("transport wrapper swallowed connection cleanup")
	}
}

func TestPreHeaderDeadlineIncludesBlockedRequestUpload(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		// Never consume the large body; the client's socket write must block.
		<-release
	}))
	defer upstream.Close()
	defer close(release)
	client := newUpstreamClient(200*time.Millisecond, time.Hour)
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL, strings.NewReader(strings.Repeat("x", 50<<20)))
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		response, err := client.Do(req)
		if response != nil {
			response.Body.Close()
		}
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upload did not reach local server")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			t.Fatalf("upload pre-header timeout = %v; parent=%v", err, ctx.Err())
		}
	case <-time.After(time.Second):
		cancel()
		<-result
		t.Fatal("blocked upload escaped pre-header deadline")
	}
}

func TestTricklingErrorBodyDoesNotDelayStatusOrFallback(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			daily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Retry-After", "23")
				w.WriteHeader(status)
				w.(http.Flusher).Flush()
				for {
					select {
					case <-r.Context().Done():
						return
					case <-time.After(20 * time.Millisecond):
						if _, err := io.WriteString(w, "credential-secret"); err != nil {
							return
						}
						w.(http.Flusher).Flush()
					}
				}
			}))
			defer daily.Close()
			prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "{}") }))
			defer prod.Close()
			p := New(config.Config{DailyEndpoint: daily.URL, ProdEndpoint: prod.URL})
			p.projectID = "project"
			p.client = newUpstreamClient(500*time.Millisecond, 100*time.Millisecond)
			defer p.client.CloseIdleConnections()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				response, err := p.postToAntigravity(ctx, "token", "/operation", "application/json", map[string]string{"project": "project"})
				if response != nil {
					response.Body.Close()
				}
				result <- err
			}()
			select {
			case err := <-result:
				if status == http.StatusServiceUnavailable || status == http.StatusNotFound {
					if err != nil {
						t.Fatalf("fallback delayed or failed: %v", err)
					}
				} else {
					var failure *upstreamError
					if !errors.As(err, &failure) || failure.Status != status || failure.Headers.Get("Retry-After") != "23" {
						t.Fatalf("rejection metadata lost: %v", err)
					}
				}
			case <-time.After(time.Second):
				cancel()
				<-result
				t.Fatal("trickling rejection delayed status or endpoint fallback")
			}
			if p.projectID != "project" {
				t.Fatal("unclassified trickling rejection invalidated project")
			}
		})
	}
}

func TestFiniteRPCDeadlineIncludesActiveResponseBody(t *testing.T) {
	for _, path := range []string{"/v1internal:retrieveUserQuotaSummary", "/v1internal:loadCodeAssist", "/v1internal:onboardUser", "/v1internal:fetchAvailableModels"} {
		t.Run(path, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for {
					if _, err := io.WriteString(w, " "); err != nil {
						return
					}
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
						return
					case <-time.After(20 * time.Millisecond):
					}
				}
			}))
			defer upstream.Close()
			p := New(config.Config{DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
			p.client = newUpstreamClient(time.Second, 200*time.Millisecond)
			defer p.client.CloseIdleConnections()
			p.rpcTimeout = 100 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if path == "/v1internal:retrieveUserQuotaSummary" {
				p.cfg.AccessToken = "token"
				_, err := p.QuotaFetcher().Fetch(ctx)
				if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
					t.Fatalf("quota finite body escaped its own deadline: %v; caller=%v", err, ctx.Err())
				}
				return
			}
			response, err := p.postToAntigravity(ctx, "token", path, "application/json", map[string]string{})
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			_, err = io.ReadAll(response.Body)
			if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				t.Fatalf("active finite body escaped its own deadline: %v; caller=%v", err, ctx.Err())
			}
		})
	}
}

func TestSharedDiscoveryRetainsTotalDeadline(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for {
			if _, err := io.WriteString(w, " "); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}))
	defer upstream.Close()
	p := New(config.Config{DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
	p.client = newUpstreamClient(time.Second, 200*time.Millisecond)
	defer p.client.CloseIdleConnections()
	p.rpcTimeout = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := p.getProjectID(ctx, "token")
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("shared discovery lost total deadline: %v; caller=%v", err, ctx.Err())
	}
}

func TestFiniteDeadlineSpansAllFallbackAttempts(t *testing.T) {
	daily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(140 * time.Millisecond):
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer daily.Close()
	prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for {
			if _, err := io.WriteString(w, " "); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}))
	defer prod.Close()
	p := New(config.Config{DailyEndpoint: daily.URL, ProdEndpoint: prod.URL})
	p.client = newUpstreamClient(time.Second, 200*time.Millisecond)
	defer p.client.CloseIdleConnections()
	p.rpcTimeout = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	response, err := p.postToAntigravity(ctx, "token", "/operation", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, err = io.ReadAll(response.Body)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) >= 300*time.Millisecond {
		t.Fatalf("fallback renewed finite deadline: %v after %v", err, time.Since(started))
	}
}

func TestSharedDiscoveryDeadlineSpansLoadAndOnboarding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(70 * time.Millisecond):
		}
		if r.URL.Path == "/v1internal:loadCodeAssist" {
			io.WriteString(w, `{}`)
			return
		}
		io.WriteString(w, `{"done":true,"response":{"cloudaicompanionProject":"project"}}`)
	}))
	defer upstream.Close()
	p := New(config.Config{DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
	p.rpcTimeout = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if id, err := p.getProjectID(ctx, "token"); id != "" || !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("compound discovery renewed total deadline: %q, %v; caller=%v", id, err, ctx.Err())
	}
}

func TestOnboardingTotalDeadlineIncludesPollingIntervals(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"done":false}`)
	}))
	defer upstream.Close()
	p := New(config.Config{DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
	p.rpcTimeout = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if id, err := p.pollOnboarding(ctx, "token", nil, 3, 70*time.Millisecond); id != "" || !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("onboarding intervals escaped total deadline: %q, %v; caller=%v", id, err, ctx.Err())
	}
}
