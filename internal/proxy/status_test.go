package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-proxy/internal/config"
	quotastatus "antigravity-proxy/internal/status"
)

func TestStatusEndpointsRequireLocalKeyAndGet(t *testing.T) {
	proxy := New(config.Config{APIKey: "local-key"})
	proxy.StatusHandler = quotastatus.NewService(config.Config{}, nil)
	for _, path := range []string{"/status/limit", "/status/usage"} {
		for _, authenticated := range []bool{false, true} {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, path, nil)
			if authenticated {
				request.Header.Set("Authorization", "Bearer local-key")
			}
			proxy.ServeHTTP(recorder, request)
			want := http.StatusUnauthorized
			if authenticated {
				want = http.StatusServiceUnavailable
			}
			if recorder.Code != want {
				t.Fatalf("%s authenticated=%t status=%d, want %d", path, authenticated, recorder.Code, want)
			}
		}
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, path, nil)
		request.Header.Set("x-goog-api-key", "local-key")
		proxy.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodGet {
			t.Fatalf("%s POST response = %d %v", path, recorder.Code, recorder.Header())
		}
	}
}

func TestQuotaStatusCompositionSharesAccountAndNativeTransport(t *testing.T) {
	var quotaCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer shared-cached-token" {
			t.Error("composed handlers did not share the account cache")
		}
		switch r.URL.Path {
		case "/v1internal:retrieveUserQuotaSummary":
			quotaCalls.Add(1)
			_, _ = io.WriteString(w, quotaTestResponse)
		case "/v1internal:fetchAvailableModels":
			_, _ = io.WriteString(w, `{"models":{}}`)
		default:
			t.Errorf("unexpected discovery/provisioning operation %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	cfg := config.Config{APIKey: "local-key", RefreshToken: "unused-refresh-token", ProjectID: "shared-project", DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL, QuotaHistoryPath: filepath.Join(t.TempDir(), "usage.jsonl")}
	p := New(cfg)
	p.cachedToken, p.tokenExpiresAt = "shared-cached-token", time.Now().Add(time.Hour)
	service := quotastatus.NewService(cfg, p.QuotaFetcher().Fetch)
	p.StatusHandler = service
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := service.Close(); err != nil {
			t.Errorf("close status: %v", err)
		}
	}()
	for deadline := time.Now().Add(3 * time.Second); ; {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/status/limit?key=local-key", nil))
		if w.Code == http.StatusOK {
			var body struct {
				Polling quotastatus.PollInfo `json:"polling"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Polling.IntervalSeconds != 300 {
				t.Fatalf("composed default polling response = %s, error = %v", w.Body.String(), err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("composed status never published: %d %s", w.Code, w.Body.String())
		}
		time.Sleep(time.Millisecond)
	}
	for _, path := range []string{"/status/usage?key=local-key&pool=gemini&window=weekly&limit=1", "/models?key=local-key"} {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("composed %s returned %d: %s", path, w.Code, w.Body.String())
		}
	}
	if quotaCalls.Load() != 1 {
		t.Fatalf("default schedule made %d quota requests, want one immediate poll", quotaCalls.Load())
	}
}
