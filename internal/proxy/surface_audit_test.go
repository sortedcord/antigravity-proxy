package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-proxy/internal/config"
)

func awaitSurface(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for HTTP request")
	}
}

type unreadGenerationBody struct{ reads int }

func (b *unreadGenerationBody) Read([]byte) (int, error) { b.reads++; panic("generation body read") }
func (b *unreadGenerationBody) Close() error             { return nil }

func TestGenerationCapacityRejectsBeforeReadAndReleasesOnCancel(t *testing.T) {
	entered := make(chan struct{}, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		<-r.Context().Done()
	}))
	defer upstream.Close()
	p := generationTestProxy(upstream.URL)
	contexts := make([]context.CancelFunc, 2)
	done := make([]chan struct{}, 2)
	for i := range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		contexts[i] = cancel
		defer cancel()
		done[i] = make(chan struct{})
		go func(i int) {
			defer close(done[i])
			p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1beta/models/model:generateContent", strings.NewReader(`{}`)).WithContext(ctx))
		}(i)
		awaitSurface(t, entered)
	}
	proxyServer := httptest.NewServer(p)
	defer proxyServer.Close()
	conn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// Keep every slot busy and withhold a body smaller than net/http's 256 KiB
	// automatic drain threshold. The response must not wait for body bytes.
	_, err = io.WriteString(conn, "POST /v1beta/models/model:generateContent HTTP/1.1\r\nHost: proxy\r\nContent-Length: 131072\r\nContent-Type: application/json\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	rejected, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("saturated HTTP/1 response waited for withheld body: %v", err)
	}
	rejectedBody, err := io.ReadAll(rejected.Body)
	_ = rejected.Body.Close()
	if err != nil || rejected.StatusCode != 429 || rejected.Header.Get("Retry-After") != "1" || !rejected.Close {
		t.Fatalf("saturation = %d %v body=%s err=%v", rejected.StatusCode, rejected.Header, rejectedBody, err)
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatalf("saturated connection was not promptly closed: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/model:generateContent", nil)
	request.Body = &unreadGenerationBody{}
	contexts[0]()
	awaitSurface(t, done[0])
	// A panic after admission must release the newly available slot too.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected body panic")
			}
		}()
		p.ServeHTTP(httptest.NewRecorder(), request)
	}()
	invalid := httptest.NewRecorder()
	p.ServeHTTP(invalid, httptest.NewRequest(http.MethodPost, "/v1beta/models/model:generateContent", strings.NewReader(`[]`)))
	if invalid.Code != 400 {
		t.Fatalf("slot was not released after panic: %d", invalid.Code)
	}
	contexts[1]()
	awaitSurface(t, done[1])
}

func TestConfiguredGenerationCapacity(t *testing.T) {
	entered := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		<-r.Context().Done()
	}))
	defer upstream.Close()
	p := New(config.Config{AccessToken: "token", ProjectID: "project", DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL, MaxConcurrentGenerations: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1beta/models/model:generateContent", strings.NewReader(`{}`)).WithContext(ctx))
	}()
	awaitSurface(t, entered)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1beta/models/model:generateContent", strings.NewReader(`{}`)))
	if w.Code != 429 {
		t.Fatalf("configured bound = %d", w.Code)
	}
	cancel()
	awaitSurface(t, done)
}

func TestAccessLogRequestIDEndpointFlushAndSecrecy(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	envelopeIDs := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var envelope struct {
			RequestID string `json:"requestId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&envelope)
		envelopeIDs <- envelope.RequestID
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"parts\":[{\"text\":\"body-secret\"}]}}]}}\n\n")
	}))
	defer upstream.Close()
	p := generationTestProxy(upstream.URL)
	r := httptest.NewRequest(http.MethodPost, "/v1beta/models/model:streamGenerateContent?alt=sse&key=query-secret", strings.NewReader(`{"contents":[{"parts":[{"text":"input-secret"}]}]}`))
	r.Header.Set("Authorization", "Bearer header-secret")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	var envelopeID string
	select {
	case envelopeID = <-envelopeIDs:
	case <-time.After(5 * time.Second):
		t.Fatal("generation did not reach upstream")
	}
	if w.Code != 200 || !w.Flushed || w.Header().Get("X-Request-ID") == "" || w.Header().Get("X-Request-ID") != envelopeID {
		t.Fatalf("stream = %d flushed=%v id=%q envelope=%q", w.Code, w.Flushed, w.Header().Get("X-Request-ID"), envelopeID)
	}
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"route", "model", "status", "duration", "request_id", "endpoint", "aborted"} {
		if _, ok := entry[key]; !ok {
			t.Fatalf("missing log field %s: %s", key, logs.String())
		}
	}
	if entry["endpoint"] != upstream.URL || entry["request_id"] != envelopeID || entry["aborted"] != false {
		t.Fatalf("access fields = %v", entry)
	}
	for _, secret := range []string{"body-secret", "input-secret", "query-secret", "header-secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("access log leaked %s", secret)
		}
	}
}

func TestAccessLogRecordsStreamAbortWithoutSuppressingPanic(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial-secret\"}]}}]}}\n\n")
	}))
	defer upstream.Close()
	p := generationTestProxy(upstream.URL)
	func() {
		defer func() {
			if value := recover(); value != http.ErrAbortHandler {
				t.Errorf("panic = %v", value)
			}
		}()
		p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1beta/models/model:streamGenerateContent?alt=sse", strings.NewReader(`{}`)))
	}()
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["aborted"] != true || entry["status"] != float64(200) || strings.Contains(logs.String(), "partial-secret") {
		t.Fatalf("abort log = %s", logs.String())
	}
}

func TestSanitizedErrorsAcrossHTTPRoutes(t *testing.T) {
	for _, body := range []string{"plain credential-secret", `{"error":{"message":"credential-secret","details":[{"token":"credential-secret"}]},"access_token":"credential-secret"}`} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "12")
			w.Header().Set("Set-Cookie", "credential-secret")
			w.Header().Set("X-Secret", "credential-secret")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, body)
		}))
		p := generationTestProxy(upstream.URL)
		for _, path := range []string{"/models", "/v1beta/models", "/v1beta/models/model:generateContent"} {
			method := http.MethodGet
			if strings.Contains(path, ":") {
				method = http.MethodPost
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(`{}`)))
			if w.Code != 429 || strings.Contains(w.Body.String(), "credential-secret") || w.Header().Get("Retry-After") != "12" || w.Header().Get("Set-Cookie") != "" || w.Header().Get("X-Secret") != "" {
				t.Fatalf("unsafe error %s: %d %v %s", path, w.Code, w.Header(), w.Body.String())
			}
		}
		upstream.Close()
	}
	w := httptest.NewRecorder()
	writeGeminiUpstreamError(w, errors.New("transport credential-secret"))
	if strings.Contains(w.Body.String(), "credential-secret") {
		t.Fatal(w.Body.String())
	}
}

func waitCatalogWaiters(t *testing.T, p *Proxy, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.catalogMu.Lock()
		flight := p.catalog.flight
		ready := flight != nil && flight.waiters == n
		p.catalogMu.Unlock()
		if ready {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("catalog callers did not join shared fetch")
}

func TestModelCatalogSharesFetchAndWaitersCancelIndependently(t *testing.T) {
	var calls atomic.Int32
	entered, release := make(chan struct{}, 1), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
			_, _ = io.WriteString(w, `{"models":{"model":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI","maxTokens":9007199254740993}}}`)
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	p := generationTestProxy(upstream.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done1, done2 := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done1)
		p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1beta/models", nil).WithContext(ctx))
	}()
	awaitSurface(t, entered)
	second := httptest.NewRecorder()
	go func() {
		defer close(done2)
		p.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/v1beta/models/model", nil))
	}()
	waitCatalogWaiters(t, p, 2)
	cancel()
	awaitSurface(t, done1)
	close(release)
	awaitSurface(t, done2)
	if second.Code != 200 || calls.Load() != 1 || !strings.Contains(second.Body.String(), "9007199254740993") {
		t.Fatalf("shared lookup = %d calls=%d body=%s", second.Code, calls.Load(), second.Body.String())
	}
	cached := catalogTestRequest(p, "/v1beta/models")
	if cached.Code != 200 || calls.Load() != 1 {
		t.Fatalf("cache miss: %d calls=%d", cached.Code, calls.Load())
	}
}

func TestModelCatalogCancelsLastWaiterAndDoesNotCacheFailure(t *testing.T) {
	entered, canceled := make(chan struct{}, 2), make(chan struct{}, 2)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			entered <- struct{}{}
			<-r.Context().Done()
			canceled <- struct{}{}
			return
		}
		_, _ = io.WriteString(w, `{"models":{}}`)
	}))
	defer upstream.Close()
	p := generationTestProxy(upstream.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1beta/models", nil).WithContext(ctx))
	}()
	awaitSurface(t, entered)
	cancel()
	awaitSurface(t, done)
	awaitSurface(t, canceled)
	if w := catalogTestRequest(p, "/v1beta/models"); w.Code != 200 || calls.Load() != 2 {
		t.Fatalf("canceled fetch was cached: %d calls=%d", w.Code, calls.Load())
	}
}

func TestModelCatalogAccountProjectAndIsolation(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"models":{"model":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI","maxTokens":8123}}}`)
	}))
	defer upstream.Close()
	p := generationTestProxy(upstream.URL)
	models, err := p.cachedGeminiModels(context.Background(), "token-a", "project-a")
	if err != nil {
		t.Fatal(err)
	}
	models[0].Name = "mutated"
	models[0].InputTokenLimit[0] = '9'
	next, err := p.cachedGeminiModels(context.Background(), "token-a", "project-a")
	if err != nil || next[0].Name != "models/model" || string(next[0].InputTokenLimit) != "8123" || calls.Load() != 1 {
		t.Fatalf("mutable cache: %v %v calls=%d", next, err, calls.Load())
	}
	_, _ = p.cachedGeminiModels(context.Background(), "token-b", "project-a")
	_, _ = p.cachedGeminiModels(context.Background(), "token-b", "project-b")
	if calls.Load() != 3 {
		t.Fatalf("account/project cache separation calls=%d", calls.Load())
	}
}

func TestModelCatalogTTLExpiresThroughHTTP(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, `{"models":{"model":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI","displayName":"First"}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"models":{"model":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI","displayName":"Updated"}}}`)
	}))
	defer upstream.Close()
	p := generationTestProxy(upstream.URL)
	advance := catalogTestClock(p)
	first := readCatalogTestList(t, p, "")
	if got := catalogTestString(t, first.Models[0], "displayName"); got != "First" {
		t.Fatalf("first catalog = %q", got)
	}
	advance(modelCatalogTTL - time.Nanosecond)
	beforeExpiry := readCatalogTestList(t, p, "")
	if got := catalogTestString(t, beforeExpiry.Models[0], "displayName"); got != "First" || calls.Load() != 1 {
		t.Fatalf("catalog expired early: name=%q calls=%d", got, calls.Load())
	}
	advance(time.Nanosecond)
	afterExpiry := readCatalogTestList(t, p, "")
	if got := catalogTestString(t, afterExpiry.Models[0], "displayName"); got != "Updated" || calls.Load() != 2 {
		t.Fatalf("expired catalog was not refreshed: name=%q calls=%d", got, calls.Load())
	}
	readCatalogTestList(t, p, "")
	if calls.Load() != 2 {
		t.Fatalf("refreshed catalog was not cached: calls=%d", calls.Load())
	}
}

func TestModelCatalogHTTPFailureIsNotCached(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"temporarily unavailable"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"models":{"model":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"}}}`)
	}))
	defer upstream.Close()
	p := generationTestProxy(upstream.URL)
	if response := catalogTestRequest(p, "/v1beta/models"); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed catalog status = %d: %s", response.Code, response.Body.String())
	}
	for range 2 {
		list := readCatalogTestList(t, p, "")
		if len(list.Models) != 1 || calls.Load() != 2 {
			t.Fatalf("HTTP failure cached or recovered catalog uncached: models=%v calls=%d", list.Models, calls.Load())
		}
	}
}

func TestModelCatalogProjectRejectionInvalidatesThroughHTTP(t *testing.T) {
	var discoveries, catalogs atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			discoveries.Add(1)
			// Rediscover the same ID: a different cache key must not be required
			// for project invalidation to refresh the catalog.
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"catalog-project"}`)
		case "/v1internal:fetchAvailableModels":
			if catalogs.Add(1) == 1 {
				_, _ = io.WriteString(w, `{"models":{"model":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI","displayName":"First"}}}`)
				return
			}
			_, _ = io.WriteString(w, `{"models":{"model":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI","displayName":"After rejection"}}}`)
		case "/v1internal:generateContent":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"PROJECT_INVALID","domain":"googleapis.com","metadata":{"project":"catalog-project"}}]}}`)
		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	p := New(config.Config{AccessToken: "token", DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
	first := readCatalogTestList(t, p, "")
	if got := catalogTestString(t, first.Models[0], "displayName"); got != "First" {
		t.Fatalf("first catalog = %q", got)
	}
	readCatalogTestList(t, p, "")
	if discoveries.Load() != 1 || catalogs.Load() != 1 {
		t.Fatalf("initial project/catalog not reused: discoveries=%d catalogs=%d", discoveries.Load(), catalogs.Load())
	}
	rejected := httptest.NewRecorder()
	p.ServeHTTP(rejected, httptest.NewRequest(http.MethodPost, "/v1beta/models/model:generateContent", strings.NewReader(`{}`)))
	if rejected.Code != http.StatusForbidden {
		t.Fatalf("project rejection = %d: %s", rejected.Code, rejected.Body.String())
	}
	refreshed := readCatalogTestList(t, p, "")
	if got := catalogTestString(t, refreshed.Models[0], "displayName"); got != "After rejection" || discoveries.Load() != 2 || catalogs.Load() != 2 {
		t.Fatalf("project rejection did not refresh catalog: name=%q discoveries=%d catalogs=%d", got, discoveries.Load(), catalogs.Load())
	}
}
