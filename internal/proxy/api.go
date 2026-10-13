// Package proxy exposes native Gemini APIs backed by Google Antigravity.
package proxy

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"antigravity-proxy/internal/httpjson"
)

// ServeHTTP serves health, model discovery, native Gemini generation, and quota status.
// Health is unauthenticated and does not probe Google; other supported routes
// require the local API key when one is configured.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.serveWithAccessLog(w, r)
}

func (p *Proxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	route := resolveRoute(r.URL.Path)
	if route.kind == routeHealth && r.Method == http.MethodGet {
		httpjson.Write(w, http.StatusOK, map[string]any{"status": "ok", "credential_configured": p.hasCredentials()})
		return
	}
	if route.kind == routeUnknown || route.kind == routeHealth || (route.kind == routeRawModels && r.Method != http.MethodGet) {
		http.NotFound(w, r)
		return
	}
	if route.kind == routeLogin {
		w.Header().Set("Cache-Control", "no-store")
		if p.cfg.APIKey == "" {
			rejectUnreadBody(w, r)
			httpjson.Write(w, http.StatusServiceUnavailable, map[string]string{"error": "API_KEY must be configured on the server to use /config/login"})
			return
		}
	}
	if !p.authorized(w, r) {
		return
	}
	switch route.kind {
	case routeRawModels:
		if !p.hasCredentials() {
			httpjson.Write(w, http.StatusServiceUnavailable, map[string]string{"error": "Google authentication unavailable"})
			return
		}
		p.handleModels(w, r)
	case routeLimit, routeUsage:
		if p.StatusHandler == nil {
			rejectUnreadBody(w, r)
			w.Header().Set("Cache-Control", "no-store")
			httpjson.Write(w, http.StatusServiceUnavailable, map[string]string{"error": "quota status is not configured"})
			return
		}
		p.StatusHandler.ServeHTTP(w, r)
	case routeAccount:
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			httpjson.Write(w, http.StatusMethodNotAllowed, map[string]string{"error": "status endpoints require GET"})
			return
		}
		p.handleStatusAccount(w, r)
	case routeLogin:
		if p.LoginHandler == nil {
			rejectUnreadBody(w, r)
			httpjson.Write(w, http.StatusServiceUnavailable, map[string]string{"error": "runtime login is not configured"})
			return
		}
		p.LoginHandler.ServeHTTP(w, r)
	case routeNativeList, routeNativeModel:
		p.handleGeminiRoute(w, r, route)
	}
}

// authorized checks the local proxy key, not Google's OAuth credential.
// The first nonempty credential source wins; a mismatch does not fall back
// to another header or query parameter.
func (p *Proxy) authorized(w http.ResponseWriter, r *http.Request) bool {
	if p.cfg.APIKey == "" {
		return true
	}
	provided := strings.TrimSpace(r.Header.Get("x-goog-api-key"))
	if provided == "" {
		provided = strings.TrimSpace(r.URL.Query().Get("key"))
	}
	if provided == "" {
		provided = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	if provided == "" {
		if authorization := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
			provided = strings.TrimSpace(authorization[len("Bearer "):])
		}
	}
	if len(provided) != len(p.cfg.APIKey) || subtle.ConstantTimeCompare([]byte(provided), []byte(p.cfg.APIKey)) != 1 {
		if strings.HasPrefix(r.URL.Path, "/v1beta/") {
			writeGeminiError(w, http.StatusUnauthorized, "invalid API key")
		} else {
			httpjson.Write(w, http.StatusUnauthorized, map[string]string{"error": "invalid API key"})
		}
		return false
	}
	return true
}

// Reject without waiting for HTTP/1's automatic unread-body drain.
func rejectUnreadBody(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor == 1 && r.Body != nil && r.Body != http.NoBody {
		w.Header().Set("Connection", "close")
		_ = http.NewResponseController(w).SetReadDeadline(time.Now())
	}
}

func (p *Proxy) handleModels(w http.ResponseWriter, r *http.Request) {
	token, err := p.accessToken(r.Context())
	if err != nil {
		httpjson.Write(w, http.StatusServiceUnavailable, map[string]string{"error": "Google authentication unavailable"})
		return
	}
	projectID, err := p.getProjectID(r.Context(), token)
	if err != nil {
		writeGeminiUpstreamError(w, err)
		return
	}
	body, code, err := p.fetchRawModelCatalog(r.Context(), token, projectID)
	if err != nil {
		writeGeminiUpstreamError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func (p *Proxy) handleStatusAccount(w http.ResponseWriter, r *http.Request) {
	if !p.hasCredentials() {
		httpjson.Write(w, http.StatusServiceUnavailable, map[string]string{"error": "Google authentication unavailable"})
		return
	}
	details, err := p.AccountDetails(r.Context())
	if err != nil {
		httpjson.Write(w, http.StatusServiceUnavailable, map[string]string{"error": "Google authentication unavailable"})
		return
	}
	httpjson.Write(w, http.StatusOK, details)
}

func (p *Proxy) handleGeminiRoute(w http.ResponseWriter, r *http.Request, route apiRoute) {
	if route.kind == routeNativeList {
		if r.Method != http.MethodGet {
			writeGeminiError(w, http.StatusMethodNotAllowed, "model listing requires GET")
			return
		}
		p.handleGeminiModels(w, r)
		return
	}
	model, action, hasAction := route.model, route.action, route.hasAction
	if model == "" || strings.ContainsAny(model, "/:") || strings.Contains(action, ":") {
		writeGeminiError(w, http.StatusNotFound, "model endpoint not found")
		return
	}
	if !hasAction {
		if r.Method != http.MethodGet {
			writeGeminiError(w, http.StatusMethodNotAllowed, "model lookup requires GET")
			return
		}
		p.handleGeminiModel(w, r, model)
		return
	}
	if action != "generateContent" && action != "streamGenerateContent" {
		writeGeminiError(w, http.StatusNotImplemented, "model operation is not supported by Antigravity Proxy")
		return
	}
	if r.Method != http.MethodPost {
		writeGeminiError(w, http.StatusMethodNotAllowed, "content generation requires POST")
		return
	}
	stream := action == "streamGenerateContent"
	if stream && r.URL.Query().Get("alt") != "sse" {
		writeGeminiError(w, http.StatusBadRequest, "streamGenerateContent requires alt=sse")
		return
	}
	p.handleGenerateContent(w, r, model, stream)
}

func writeGeminiError(w http.ResponseWriter, code int, message string) {
	status := "INTERNAL"
	switch code {
	case http.StatusBadRequest:
		status = "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		status = "UNAUTHENTICATED"
	case http.StatusForbidden:
		status = "PERMISSION_DENIED"
	case http.StatusNotFound:
		status = "NOT_FOUND"
	case http.StatusRequestEntityTooLarge, http.StatusTooManyRequests:
		status = "RESOURCE_EXHAUSTED"
	case http.StatusMethodNotAllowed, http.StatusNotImplemented:
		status = "UNIMPLEMENTED"
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		status = "UNAVAILABLE"
	case http.StatusGatewayTimeout:
		status = "DEADLINE_EXCEEDED"
	}
	httpjson.Write(w, code, map[string]any{"error": map[string]any{"code": code, "message": message, "status": status}})
}

// writeGeminiUpstreamError exposes HTTP status and validated retry metadata,
// never provider bodies, credentials or arbitrary transport error strings.
func writeGeminiUpstreamError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	var upstream *upstreamError
	if errors.As(err, &upstream) {
		if upstream.Status >= 400 && upstream.Status <= 599 {
			status = upstream.Status
		}
		for key, values := range safeUpstreamHeaders(upstream.Headers) {
			w.Header()[key] = values
		}
	}
	writeGeminiError(w, status, "Google upstream request failed")
}
