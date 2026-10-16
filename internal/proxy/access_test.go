package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type surfaceControllerWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
	flushErr error
}

func (w *surfaceControllerWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func (w *surfaceControllerWriter) FlushError() error { w.Flush(); return w.flushErr }

func TestAccessWriterPreservesResponseController(t *testing.T) {
	failure := errors.New("flush failure")
	underlying := &surfaceControllerWriter{ResponseRecorder: httptest.NewRecorder(), flushErr: failure}
	wrapped := &accessResponseWriter{ResponseWriter: underlying}
	controller := http.NewResponseController(wrapped)
	deadline := time.Now().Add(time.Minute)
	if err := controller.SetWriteDeadline(deadline); err != nil || underlying.deadline != deadline {
		t.Fatalf("deadline forwarding = %v %v", err, underlying.deadline)
	}
	if err := controller.Flush(); !errors.Is(err, failure) || wrapped.status != 200 || !underlying.Flushed {
		t.Fatalf("flush forwarding = %v status=%d flushed=%v", err, wrapped.status, underlying.Flushed)
	}
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
