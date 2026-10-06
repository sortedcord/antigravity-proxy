package proxy

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/health" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "credential_configured": p.cfg.AccessToken != "" || p.cfg.RefreshToken != ""})
	case r.URL.Path == "/models" && r.Method == http.MethodGet:
		if !p.authorized(w, r) {
			return
		}
		p.handleModels(w, r)
	case r.URL.Path == "/v1beta/models" || strings.HasPrefix(r.URL.Path, "/v1beta/models/"):
		if !p.authorized(w, r) {
			return
		}
		p.handleGeminiRoute(w, r)
	default:
		http.NotFound(w, r)
	}
}

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
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid API key"})
		}
		return false
	}
	return true
}

func (p *Proxy) handleModels(w http.ResponseWriter, r *http.Request) {
	token, err := p.accessToken(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	projectID, err := p.getProjectID(r.Context(), token)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	resp, err := p.postToAntigravity(r.Context(), token, "/v1internal:fetchAvailableModels", "application/json", map[string]any{"project": projectID})
	if err != nil {
		status := http.StatusBadGateway
		var upstream *upstreamError
		if errors.As(err, &upstream) && upstream.Status >= 400 && upstream.Status <= 599 {
			status = upstream.Status
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	const maxModelListSize = 8 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelListSize+1))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "read model list from Cloud Code"})
		return
	}
	if len(body) > maxModelListSize || !json.Valid(body) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "invalid model list from Cloud Code"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (p *Proxy) handleGeminiRoute(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1beta/models" {
		if r.Method != http.MethodGet {
			writeGeminiError(w, http.StatusMethodNotAllowed, "model listing requires GET")
			return
		}
		p.handleGeminiModels(w, r)
		return
	}
	resource := strings.TrimPrefix(r.URL.Path, "/v1beta/models/")
	model, action, hasAction := strings.Cut(resource, ":")
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
	writeJSON(w, code, map[string]any{"error": map[string]any{"code": code, "message": message, "status": status}})
}

func writeGeminiUpstreamError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	var upstream *upstreamError
	if errors.As(err, &upstream) {
		if upstream.Status >= 400 && upstream.Status <= 599 {
			status = upstream.Status
		}
		var body struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(upstream.Body), &body) == nil && len(body.Error) > 0 && body.Error[0] == '{' {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, upstream.Body)
			return
		}
	}
	writeGeminiError(w, status, err.Error())
}
