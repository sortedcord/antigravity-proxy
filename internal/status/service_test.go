package status

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/quota"
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
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		var response limitResponse
		code := requestStatus(t, client, address+"/status/limit", key, &response)
		if code == http.StatusOK && response.Pools.Gemini.FiveHour.RemainingFraction != nil && *response.Pools.Gemini.FiveHour.RemainingFraction == fraction {
			return response
		}
		select {
		case <-deadline.C:
			t.Fatalf("quota observation %.2f not published: status=%d response=%+v", fraction, code, response)
		case <-tick.C:
		}
	}
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
	if first.Pools.ThirdParty.FiveHour.RemainingFraction == nil || *first.Pools.ThirdParty.FiveHour.RemainingFraction != 0 || first.Pools.ThirdParty.FiveHour.Status != "available" {
		t.Fatalf("exhausted third-party quota was not preserved: %+v", first.Pools.ThirdParty.FiveHour)
	}
	if first.Pools.Gemini.Weekly.RemainingPercent == nil || *first.Pools.Gemini.Weekly.RemainingPercent != 60 || first.Pools.ThirdParty.Weekly.RemainingFraction == nil || *first.Pools.ThirdParty.Weekly.RemainingFraction != 0.9 {
		t.Fatalf("pool/window values were mixed: %+v", first.Pools)
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
			if code != http.StatusOK || !retained.Stale || !retained.ObservedAt.Equal(second.ObservedAt) || retained.Pools.Gemini.FiveHour.RemainingFraction == nil || *retained.Pools.Gemini.FiveHour.RemainingFraction != 0.50 {
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

	// Block the new fetch: queries must come from the reloaded history, not a
	// successful re-fetch that could conceal lost persistence.
	block.Store(true)
	cfg.QuotaPollIntervalSeconds = 3600
	restarted := newUpstreamService(cfg)
	restartCtx, restartCancel := context.WithCancel(context.Background())
	defer restartCancel()
	if err := restarted.Start(restartCtx); err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restartServer := httptest.NewServer(restarted)
	defer restartServer.Close()
	restartClient := restartServer.Client()
	restartClient.Timeout = time.Second
	loaded := waitForLimit(t, restartClient, restartServer.URL, cfg.APIKey, 0.50)
	if !loaded.ObservedAt.Equal(second.ObservedAt) {
		t.Fatalf("restart replaced stored observation timestamp: got %s, want %s", loaded.ObservedAt, second.ObservedAt)
	}
	parameters := url.Values{"pool": {"gemini"}, "window": {"weekly"}, "from": {first.ObservedAt.Format(time.RFC3339Nano)}, "to": {second.ObservedAt.Format(time.RFC3339Nano)}, "order": {"asc"}, "limit": {"1"}}
	var history Result
	if code := requestStatus(t, restartClient, restartServer.URL+"/status/usage?"+parameters.Encode(), cfg.APIKey, &history); code != http.StatusOK {
		t.Fatalf("history status=%d", code)
	}
	if history.Total != 2 || len(history.Entries) != 1 || history.NextOffset == nil || *history.NextOffset != 1 {
		t.Fatalf("wrong historical pagination: %+v", history)
	}
	entry := history.Entries[0]
	if entry.Pool != "gemini" || entry.Window.Window != "weekly" || entry.BucketID != "gemini-weekly" || !entry.ObservedAt.Equal(first.ObservedAt) || entry.RemainingFraction == nil || *entry.RemainingFraction != 0.60 {
		t.Fatalf("wrong selected historical measurement: %+v", entry)
	}
	parameters.Set("offset", "1")
	if code := requestStatus(t, restartClient, restartServer.URL+"/status/usage?"+parameters.Encode(), cfg.APIKey, &history); code != http.StatusOK {
		t.Fatalf("next page status=%d", code)
	}
	if len(history.Entries) != 1 || history.NextOffset != nil || !history.Entries[0].ObservedAt.Equal(second.ObservedAt) || *history.Entries[0].RemainingFraction != 0.55 {
		t.Fatalf("wrong next historical measurement: %+v", history)
	}
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
