package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"antigravity-proxy/internal/httpjson"

	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/oauth"
)

func (r *Runtime) handleLogin(w http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		address, expires, err := r.login.Begin(request.Context())
		if err != nil {
			writeLoginError(w, err)
			return
		}
		httpjson.Write(w, http.StatusOK, map[string]any{"login_url": address, "expires_at": expires.UTC().Format(time.RFC3339)})
	case http.MethodPost:
		var submission struct {
			Code  string `json:"code"`
			State string `json:"state"`
			URL   string `json:"url"`
		}
		request.Body = http.MaxBytesReader(w, request.Body, 64<<10)
		defer request.Body.Close()
		decoder := json.NewDecoder(request.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&submission); err != nil {
			httpjson.Write(w, http.StatusBadRequest, map[string]string{"error": "invalid login JSON"})
			return
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			httpjson.Write(w, http.StatusBadRequest, map[string]string{"error": "login requires one JSON object"})
			return
		}
		code, state := strings.TrimSpace(submission.Code), strings.TrimSpace(submission.State)
		if submission.URL != "" {
			if code != "" || state != "" {
				httpjson.Write(w, http.StatusBadRequest, map[string]string{"error": "provide code/state or callback URL, not both"})
				return
			}
			callback, err := url.Parse(submission.URL)
			if err != nil || callback.Scheme != "http" || callback.Hostname() != "127.0.0.1" || callback.Path != "/oauth-callback" || callback.User != nil {
				httpjson.Write(w, http.StatusBadRequest, map[string]string{"error": "invalid loopback callback URL"})
				return
			}
			code, state = callback.Query().Get("code"), callback.Query().Get("state")
		}
		if code == "" || state == "" {
			httpjson.Write(w, http.StatusBadRequest, map[string]string{"error": "both code and state are required"})
			return
		}
		user, err := r.login.Complete(request.Context(), code, state)
		if err != nil {
			writeLoginError(w, err)
			return
		}
		httpjson.Write(w, http.StatusOK, map[string]any{"status": "configured", "email": user.Email, "name": user.Name})
	default:
		rejectUnreadBody(w, request)
		w.Header().Set("Allow", "GET, POST")
		httpjson.Write(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func writeLoginError(w http.ResponseWriter, err error) {
	var commit *config.CommitError
	if errors.As(err, &commit) {
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{"error": "account activated, but credential durability could not be confirmed", "credential_configured": true, "durability_uncertain": true})
		return
	}
	var activation *activationFailure
	if errors.As(err, &activation) {
		httpjson.Write(w, http.StatusInternalServerError, map[string]string{"error": activation.message})
		return
	}
	code, message := http.StatusBadGateway, "failed to complete Google authorization"
	switch {
	case errors.Is(err, oauth.ErrInvalidState):
		code, message = http.StatusBadRequest, "invalid OAuth state"
	case errors.Is(err, oauth.ErrLoginInProgress):
		code, message = http.StatusConflict, "login completion already in progress"
	case errors.Is(err, oauth.ErrSessionAlreadyUsed):
		code, message = http.StatusConflict, "OAuth session already used"
	case errors.Is(err, oauth.ErrSessionExpired):
		code, message = http.StatusGone, "OAuth session expired"
	case errors.Is(err, oauth.ErrTokenEnvironment):
		code, message = http.StatusConflict, "remove token environment overrides to use /config/login"
	case errors.Is(err, oauth.ErrSessionManagerClosed):
		code, message = http.StatusServiceUnavailable, "login is closed"
	case errors.Is(err, context.Canceled):
		code, message = http.StatusServiceUnavailable, "login canceled"
	}
	httpjson.Write(w, code, map[string]string{"error": message})
}
