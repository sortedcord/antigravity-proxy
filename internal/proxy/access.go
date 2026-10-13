package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type accessStateKey struct{}

type accessRequestState struct {
	id       string
	mu       sync.Mutex
	endpoint string
}

// recordRequestEndpoint records only the endpoint origin, never credentials,
// paths, query strings, request bodies or provider error text.
func recordRequestEndpoint(ctx context.Context, endpoint string) {
	state, _ := ctx.Value(accessStateKey{}).(*accessRequestState)
	if state == nil {
		return
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" {
		return
	}
	state.mu.Lock()
	state.endpoint = parsed.Scheme + "://" + parsed.Host
	state.mu.Unlock()
}

func requestEndpoint(ctx context.Context) string {
	state, _ := ctx.Value(accessStateKey{}).(*accessRequestState)
	if state == nil {
		return ""
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.endpoint
}

func ensureRequestID(w http.ResponseWriter, r *http.Request) (*http.Request, error) {
	if state, ok := r.Context().Value(accessStateKey{}).(*accessRequestState); ok {
		w.Header().Set("X-Request-ID", state.id)
		return r, nil
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return r, err
	}
	state := &accessRequestState{id: "agent-" + hex.EncodeToString(id[:])}
	w.Header().Set("X-Request-ID", state.id)
	return r.WithContext(context.WithValue(r.Context(), accessStateKey{}, state)), nil
}

type accessResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *accessResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *accessResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	// Informational responses do not commit the final status.
	if status >= 100 && status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *accessResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *accessResponseWriter) FlushError() error {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *accessResponseWriter) Flush() { _ = w.FlushError() }

// accessRoute uses stable route labels rather than logging user-controlled URLs.
func accessRoute(r *http.Request) (string, string) { return resolveRoute(r.URL.Path).accessLabel() }

func (p *Proxy) serveWithAccessLog(w http.ResponseWriter, r *http.Request) {
	p.serveWithAccessLogScoped(w, r, p.accountID, "")
}

func (p *Proxy) serveWithAccessLogScoped(w http.ResponseWriter, r *http.Request, accountID, keyID string) {
	started := time.Now()
	r, err := ensureRequestID(w, r)
	if err != nil {
		writeGeminiError(w, http.StatusInternalServerError, "create request ID")
		return
	}
	writer := &accessResponseWriter{ResponseWriter: w}
	route, model := accessRoute(r)
	defer func() {
		panicValue := recover()
		status := writer.status
		if status == 0 {
			status = http.StatusOK
			if panicValue != nil {
				status = http.StatusInternalServerError
			}
		}
		attrs := []any{
			"route", route,
			"model", model,
			"status", status,
			"duration", time.Since(started),
			"request_id", w.Header().Get("X-Request-ID"),
			"endpoint", requestEndpoint(r.Context()),
			"aborted", panicValue != nil || r.Context().Err() != nil,
		}
		if accountID != "" {
			attrs = append([]any{"account", accountID}, attrs...)
		}
		if keyID != "" {
			attrs = append([]any{"key_id", keyID}, attrs...)
		}
		slog.Info("http access", attrs...)
		if panicValue != nil {
			panic(panicValue)
		}
	}()
	p.serveHTTP(writer, r)
}
