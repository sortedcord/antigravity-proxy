package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"antigravity-proxy/internal/config"
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
func (w *surfaceControllerWriter) FlushError() error { w.ResponseRecorder.Flush(); return w.flushErr }

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

func TestGenerationRawBodyPreservationAndSchemaSpans(t *testing.T) {
	original := []byte(`{"contents":[{"parts":[{"inlineData":{"data":"opaque\\\"signature"},"thoughtSignature":"raw==","number":9007199254740993}]}],"generationConfig":{"temperature":0.1234567890123456789},"future":{"nested":[{},"]}"]}}`)
	got, err := adaptGenerationBody(original)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("raw request changed: %s %v", got, err)
	}
	for _, body := range []string{
		`{"contents":[{"text":"generationConfig: { fake }"}],"generationConfig":{"responseJsonSchema":{"type":"object","properties":{"n":{"const":9007199254740993}}},"temperature":0.1234567890123456789}}`,
		`{"generationConfig":false,"generation\u0043onfig":{"responseJsonSchema":{"type":"object"}},"contents":[{"thoughtSignature":"opaque=="}]}`,
	} {
		adapted, err := adaptGenerationBody([]byte(body))
		if err != nil || strings.Contains(string(adapted), "responseJsonSchema") || !strings.Contains(string(adapted), "responseSchema") {
			t.Fatalf("schema span adaptation = %s %v", adapted, err)
		}
		for _, marker := range []string{"9007199254740993", "0.1234567890123456789", "opaque=="} {
			if strings.Contains(body, marker) && !strings.Contains(string(adapted), marker) {
				t.Fatalf("lost raw marker %s", marker)
			}
		}
	}
}

func TestGenerationEnvelopePreservesRawRequestAcrossRedirectAndFallback(t *testing.T) {
	const original = `{
		"contents": [{"parts": [{"thoughtSignature":"opaque\\\"+/==<raw>","number":9007199254740993}]}],
		"generationConfig": {"temperature":0.1234567890123456789},
		"future": {"scientific":1e+09,"escaped":"\u0061"}
	}`
	bodies := make(chan []byte, 3)
	capture := func(w http.ResponseWriter, r *http.Request) bool {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return false
		}
		if r.Method != http.MethodPost || r.ContentLength != int64(len(body)) || len(r.TransferEncoding) != 0 {
			t.Errorf("upstream framing: method=%s length=%d actual=%d transfer=%v", r.Method, r.ContentLength, len(body), r.TransferEncoding)
		}
		bodies <- body
		return true
	}
	daily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !capture(w, r) {
			return
		}
		if r.URL.Path == "/v1internal:generateContent" {
			// A 307 requires GetBody to replay the POST through http.Client.
			http.Redirect(w, r, "/replayed", http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer daily.Close()
	prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture(w, r) {
			_, _ = io.WriteString(w, `{"response":{"candidates":[]}}`)
		}
	}))
	defer prod.Close()
	const project = "project\"\\<metadata>"
	p := New(config.Config{AccessToken: "token", ProjectID: project, DailyEndpoint: daily.URL, ProdEndpoint: prod.URL})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1beta/models/model:generateContent", strings.NewReader(original)))
	if w.Code != http.StatusOK || w.Body.String() != `{"candidates":[]}` {
		t.Fatalf("generation after replay = %d: %s", w.Code, w.Body.String())
	}
	var first []byte
	for i := range 3 {
		var body []byte
		select {
		case body = <-bodies:
		case <-time.After(2 * time.Second):
			t.Fatal("missing redirect/fallback request")
		}
		if i == 0 {
			first = body
		} else if !bytes.Equal(body, first) {
			t.Fatalf("replayed envelope changed: %s", body)
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Fatalf("invalid envelope: %v", err)
		}
		if string(envelope["request"]) != original {
			t.Fatalf("raw request changed on attempt %d: %s", i, envelope["request"])
		}
		var gotProject string
		if err := json.Unmarshal(envelope["project"], &gotProject); err != nil || gotProject != project {
			t.Fatalf("metadata escaping = %q, err=%v", gotProject, err)
		}
	}
}
