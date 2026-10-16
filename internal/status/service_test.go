package status

import (
	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/quota"
	"antigravity-proxy/internal/testutil"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type limitResponse struct {
	quota.Snapshot
	Polling PollInfo `json:"polling"`
	Stale   bool     `json:"stale"`
}

func requestStatus(t *testing.T, client *http.Client, address, key string, target any) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, address, nil)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		request.Header.Set("x-api-key", key)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if target != nil {
		if err := json.NewDecoder(response.Body).Decode(target); err != nil {
			t.Fatalf("decode status response: %v", err)
		}
	}
	return response.StatusCode
}

func waitForLimit(t *testing.T, client *http.Client, address, key string, fraction float64) limitResponse {
	t.Helper()
	var response limitResponse
	testutil.Eventually(t, func() bool {
		code := requestStatus(t, client, address+"/status/limit", key, &response)
		b, found := response.Bucket("gemini-5h")
		return code == http.StatusOK && found && b.RemainingFraction != nil && *b.RemainingFraction == fraction
	}, "quota observation was not published")
	return response
}

func TestQuotaStatusPollingHistoryAndRestartHTTP(t *testing.T) {
	var calls atomic.Int64
	var block atomic.Bool
	var fail atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:retrieveUserQuotaSummary" {
			t.Errorf("quota polling attempted unrelated operation: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Drain the POST body so net/http can observe the canceled connection
		// while the restart fixture waits without sending response headers.
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		if block.Load() {
			<-r.Context().Done()
			return
		}
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		count := calls.Add(1)
		fiveHour, weekly := 0.75, 0.60
		if count > 1 {
			fiveHour, weekly = 0.50, 0.55
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h","window":"5h","remainingFraction":%g,"resetTime":"2030-01-02T03:04:05Z"},{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":%g,"resetTime":"2030-01-08T03:04:05Z"}]},{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-5h","window":"5h","remainingFraction":0,"resetTime":"2030-01-02T04:04:05Z"},{"bucketId":"3p-weekly","window":"weekly","remainingFraction":0.9,"resetTime":"2030-01-09T03:04:05Z"}]}]}`, fiveHour, weekly)
	}))
	defer upstream.Close()
	cfg := config.Config{APIKey: "local-key", AccessToken: "google-token", DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL, QuotaPollIntervalSeconds: 1, QuotaHistoryPath: filepath.Join(t.TempDir(), "usage.jsonl")}
	service := newUpstreamService(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	server := httptest.NewServer(service)
	defer server.Close()
	client := server.Client()
	client.Timeout = time.Second
	first := waitForLimit(t, client, server.URL, cfg.APIKey, 0.75)
	firstTP5h, _ := first.Bucket("3p-5h")
	if firstTP5h.RemainingFraction == nil || *firstTP5h.RemainingFraction != 0 || firstTP5h.Status != "available" {
		t.Fatalf("exhausted third-party quota was not preserved: %+v", firstTP5h)
	}
	firstGW, _ := first.Bucket("gemini-weekly")
	firstTPW, _ := first.Bucket("3p-weekly")
	if firstGW.RemainingFraction == nil || *firstGW.RemainingFraction != 0.6 || firstTPW.RemainingFraction == nil || *firstTPW.RemainingFraction != 0.9 {
		t.Fatalf("pool/window values were mixed: %+v", first.Buckets)
	}
	second := waitForLimit(t, client, server.URL, cfg.APIKey, 0.50)
	if !second.ObservedAt.After(first.ObservedAt) || second.Polling.IntervalSeconds != 1 {
		t.Fatalf("periodic observation not distinct from initial sample: first=%+v second=%+v", first, second)
	}
	fail.Store(true)
	for deadline := time.Now().Add(5 * time.Second); ; {
		var retained limitResponse
		code := requestStatus(t, client, server.URL+"/status/limit", cfg.APIKey, &retained)
		if retained.Polling.LastError != "" {
			retainedG5h, _ := retained.Bucket("gemini-5h")
			if code != http.StatusOK || !retained.Stale || !retained.ObservedAt.Equal(second.ObservedAt) || retainedG5h.RemainingFraction == nil || *retainedG5h.RemainingFraction != 0.50 {
				t.Fatalf("failed poll did not preserve and mark the last observation: %+v", retained)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("upstream failure was not exposed by the quota endpoint")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	server.Close()
	cancel()
}

func TestStatusUsageRejectsInvalidFilters(t *testing.T) {
	service := NewService(config.Config{QuotaPollIntervalSeconds: 300, QuotaHistoryPath: filepath.Join(t.TempDir(), "usage.jsonl")}, nil)
	collector, err := Open(service.historyPath, 5*time.Minute, 0, func(context.Context) (quota.Snapshot, error) { panic("read-only request unexpectedly fetched quota") })
	if err != nil {
		t.Fatal(err)
	}
	defer collector.Close()
	service.collector = collector
	for _, query := range []string{"unknown=value", "pool=gpt", "window=daily", "limit=0", "limit=1001", "offset=-1", "offset=not-an-integer", "order=random", "from=not-a-time", "from=2030-01-02T00:00:00Z&to=2030-01-01T00:00:00Z", "pool=gemini&pool=third_party", "window=", "limit=%zz"} {
		t.Run(query, func(t *testing.T) {
			w := httptest.NewRecorder()
			service.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/status/usage?"+query, nil))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("query %q status=%d, want 400: %s", query, w.Code, w.Body.String())
			}
		})
	}
	w := httptest.NewRecorder()
	service.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/status/usage?pool=gemini&window=weekly", nil))
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("empty history response = %d %v", w.Code, w.Header())
	}
	var body Result
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Entries == nil || body.Total != 0 || body.NextOffset != nil {
		t.Fatalf("empty history is not a valid exhausted page: %+v", body)
	}
}

func TestStatusLimitReportsInitialFailureWithoutInventingQuota(t *testing.T) {
	collector, err := Open(filepath.Join(t.TempDir(), "usage.jsonl"), 5*time.Minute, 0, func(context.Context) (quota.Snapshot, error) {
		return quota.Snapshot{}, fmt.Errorf("quota endpoint denied access")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer collector.Close()
	service := NewService(config.Config{}, nil)
	service.collector = collector
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := collector.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Second); ; {
		_, _, info := collector.Latest()
		if info.LastError != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("initial quota polling failure was not recorded")
		}
		time.Sleep(time.Millisecond)
	}
	w := httptest.NewRecorder()
	service.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/status/limit", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed initial quota request returned status=%d", w.Code)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, fabricated := body["pools"]; fabricated {
		t.Fatalf("unobserved quotas were fabricated: %s", w.Body.String())
	}
	var message string
	if err := json.Unmarshal(body["error"], &message); err != nil || message != "quota endpoint denied access" {
		t.Fatalf("initial failure not reported: %s", w.Body.String())
	}
}

// newUpstreamService keeps status HTTP tests independent of proxy ownership,
// using the real quota fetcher against only their loopback upstream fixture.
func newUpstreamService(cfg config.Config) *Service {
	fetcher := quota.NewFetcher(quota.Transport{
		AccessToken: func(context.Context) (string, error) { return cfg.AccessToken, nil },
		ProjectID:   func() string { return cfg.ProjectID },
		Post: func(ctx context.Context, token, path, accept string, payload any) (*http.Response, error) {
			body, err := json.Marshal(payload)
			if err != nil {
				return nil, err
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.DailyEndpoint+path, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", accept)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				return nil, err
			}
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				_ = response.Body.Close()
				return nil, fmt.Errorf("upstream HTTP %d", response.StatusCode)
			}
			return response, nil
		},
	})
	return NewService(cfg, fetcher.Fetch)
}

func awaitStatusSignal(t *testing.T, signal <-chan struct{}, failure string) {
	t.Helper()
	testutil.Await(t, signal, failure)
}

func TestServicePrepareWithoutPollingAndCloseReleasesHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prepared.jsonl")
	var calls atomic.Int64
	fetch := func(context.Context) (quota.Snapshot, error) {
		calls.Add(1)
		return observation(observationTime(), 0.5), nil
	}
	service := NewService(config.Config{QuotaHistoryPath: path}, fetch)
	defer service.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("constructor opened history: %v", err)
	}
	var nilCtx context.Context
	if err := service.Start(nilCtx); err == nil {
		t.Fatal("Start accepted nil context")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("invalid Start opened history: %v", err)
	}
	if err := service.Prepare(); err != nil {
		t.Fatal(err)
	}
	collector := service.quotaCollector()
	if err := service.Prepare(); err != nil || service.quotaCollector() != collector {
		t.Fatalf("repeated preparation replaced its collector: %v", err)
	}
	service.TriggerPoll()
	if calls.Load() != 0 {
		t.Fatal("Prepare or TriggerPoll started unrequested collection")
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if err := service.Prepare(); err == nil {
		t.Fatal("closed service prepared again")
	}
	if err := service.Start(context.Background()); err == nil {
		t.Fatal("closed service started")
	}
	if calls.Load() != 0 {
		t.Fatal("preparation fetched quota")
	}
}

func TestAccountHistoryPathsSeparate(t *testing.T) {
	base := filepath.Join(t.TempDir(), "history.jsonl")
	firstPath := AccountHistoryPath(base, "../private/account")
	secondPath := AccountHistoryPath(base, "another account")
	if firstPath != AccountHistoryPath(base, "../private/account") || firstPath == secondPath || firstPath == base || AccountHistoryPath(base, "") == base {
		t.Fatal("namespace paths are not stable and isolated")
	}
	if filepath.Dir(firstPath) != filepath.Dir(base) || filepath.Ext(firstPath) != ".jsonl" || strings.Contains(filepath.Base(firstPath), "private") {
		t.Fatalf("namespace leaked into an unsafe history path: %s", firstPath)
	}
}

func TestServiceCanceledStartAndDuplicateStart(t *testing.T) {
	var calls atomic.Int64
	service := NewService(config.Config{QuotaHistoryPath: filepath.Join(t.TempDir(), "canceled")}, func(context.Context) (quota.Snapshot, error) {
		calls.Add(1)
		return quota.Snapshot{}, nil
	})
	defer service.Close()
	if err := service.Prepare(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	awaitStatusSignal(t, service.quotaCollector().Done(), "canceled start did not stop its worker")
	if calls.Load() != 0 {
		t.Fatal("already-canceled start fetched quota")
	}
	if err := service.Start(context.Background()); err == nil {
		t.Fatal("duplicate Start succeeded after parent cancellation")
	}
}

func TestServiceConcurrentCloseCancelsAndJoinsWithoutBlockingStatusReads(t *testing.T) {
	entered := make(chan struct{})
	readAfterCancellation := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFetch := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseFetch()
	var service *Service
	service = NewService(config.Config{QuotaHistoryPath: filepath.Join(t.TempDir(), "closing")}, func(ctx context.Context) (quota.Snapshot, error) {
		close(entered)
		<-ctx.Done()
		// Fetch cleanup may use status methods: Close must not hold the service
		// mutex while joining the collector worker.
		service.TriggerPoll()
		service.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/status/limit", nil))
		close(readAfterCancellation)
		<-release
		return quota.Snapshot{}, ctx.Err()
	})
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitStatusSignal(t, entered, "initial fetch did not start")
	var closers sync.WaitGroup
	errors := make(chan error, 3)
	for range 3 {
		closers.Add(1)
		go func() {
			defer closers.Done()
			errors <- service.Close()
		}()
	}
	awaitStatusSignal(t, readAfterCancellation, "Close blocked fetch cleanup status reads")
	select {
	case err := <-errors:
		t.Fatalf("Close returned before fetch cleanup finished: %v", err)
	default:
	}
	if err := service.Prepare(); err == nil {
		t.Fatal("Prepare accepted a closing service")
	}
	if err := service.Start(context.Background()); err == nil {
		t.Fatal("Start accepted a closing service")
	}
	releaseFetch()
	joined := make(chan struct{})
	go func() { closers.Wait(); close(joined) }()
	awaitStatusSignal(t, joined, "concurrent Close calls did not join worker")
	for range 3 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	if service.quotaCollector() != nil {
		t.Fatal("closed service retained its collector")
	}
}
