package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"antigravity-proxy/internal/config"
)

func TestNonGeminiAPIRoutesAreUnsupported(t *testing.T) {
	proxy := New(config.Config{})
	for _, test := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/messages"},
		{http.MethodPost, "/v1/messages/count_tokens"},
		{http.MethodGet, "/v1/models"},
		{http.MethodPost, "/refresh-token"},
		{http.MethodPost, "/"},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			proxy.ServeHTTP(recorder, httptest.NewRequest(test.method, test.path, nil))
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
			}
		})
	}
}

func TestModelListingPassesUpstreamResponseThrough(t *testing.T) {
	const responseBody = `{"models":{"gemini-example":{"displayName":"Gemini Example"},"other-model":{"displayName":"Other"}}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1internal:fetchAvailableModels" {
			t.Errorf("upstream request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer google-token" {
			t.Errorf("upstream authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responseBody)
	}))
	defer upstream.Close()

	proxy := New(config.Config{
		AccessToken:   "google-token",
		ProjectID:     "project-id",
		DailyEndpoint: upstream.URL,
		ProdEndpoint:  upstream.URL,
	})
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/models", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := strings.TrimSpace(recorder.Body.String()); got != responseBody {
		t.Fatalf("model response = %s, want unchanged Cloud Code response %s", got, responseBody)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}
}

func TestModelListingRequiresAPIKeyWhenConfigured(t *testing.T) {
	proxy := New(config.Config{APIKey: "local-key"})
	request := httptest.NewRequest(http.MethodGet, "/models", nil)
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized model request status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}

	health := httptest.NewRecorder()
	proxy.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", health.Code, http.StatusOK)
	}
}

func TestGeminiCredentialsStaySeparateFromGoogleOAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer google-token" {
			t.Errorf("upstream credential = %q, want Google OAuth token", got)
		}
		if got := r.Header.Get("x-goog-api-key"); got != "" {
			t.Errorf("local Gemini key leaked upstream: %q", got)
		}
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}]}}`)
	}))
	defer upstream.Close()
	p := New(config.Config{APIKey: "local-key", AccessToken: "google-token", ProjectID: "project", DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
	for _, tc := range []struct {
		name, header, value, query string
		status                     int
	}{
		{"Gemini header", "x-goog-api-key", "local-key", "", http.StatusOK},
		{"Gemini query", "", "", "?key=local-key", http.StatusOK},
		{"Bearer", "Authorization", "Bearer local-key", "", http.StatusOK},
		{"legacy header", "x-api-key", "local-key", "", http.StatusOK},
		{"missing key", "", "", "", http.StatusUnauthorized},
		{"wrong native header wins", "x-goog-api-key", "wrong", "?key=local-key", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-example:generateContent"+tc.query, strings.NewReader(`{"contents":[{"parts":[{"text":"Hello"}]}]}`))
			if tc.header != "" {
				r.Header.Set(tc.header, tc.value)
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if tc.status == http.StatusUnauthorized {
				var body struct {
					Error struct {
						Code   int    `json:"code"`
						Status string `json:"status"`
					} `json:"error"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body.Error.Code != http.StatusUnauthorized || body.Error.Status != "UNAUTHENTICATED" {
					t.Fatalf("not a native Gemini authentication error: %s", w.Body.String())
				}
			}
		})
	}
}

func TestGeminiUpstreamErrorRetainsProviderDetails(t *testing.T) {
	const providerError = `{"error":{"code":429,"message":"quota exhausted","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"30s"}]}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, providerError)
	}))
	defer upstream.Close()
	p := New(config.Config{AccessToken: "google-token", ProjectID: "project", DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-example:generateContent", strings.NewReader(`{"contents":[{"parts":[{"text":"Hello"}]}]}`)))
	if w.Code != http.StatusTooManyRequests || w.Body.String() != providerError {
		t.Fatalf("provider error changed: status=%d body=%s", w.Code, w.Body.String())
	}
}
