package proxy

import (
	"context"
	"errors"
	"net/http"

	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/httpjson"
	"antigravity-proxy/internal/oauth"
)

func writeLoginError(w http.ResponseWriter, err error) {
	if errors.Is(err, config.ErrUnloadableConfig) {
		httpjson.Write(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	var commit *config.CommitError
	if errors.As(err, &commit) {
		httpjson.Write(w, http.StatusInternalServerError, map[string]any{
			"error":                 "account activated, but credential durability could not be confirmed",
			"credential_configured": true,
			"durability_uncertain":  true,
		})
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
		code, message = http.StatusConflict, "remove token environment overrides to use login"
	case errors.Is(err, oauth.ErrSessionManagerClosed):
		code, message = http.StatusServiceUnavailable, "login is closed"
	case errors.Is(err, oauth.ErrAccountIdentityMismatch):
		code, message = http.StatusConflict, err.Error()
	case errors.Is(err, oauth.ErrGoogleAccountAlreadyLinked):
		code, message = http.StatusConflict, err.Error()
	case errors.Is(err, oauth.ErrSessionNotFound):
		code, message = http.StatusNotFound, err.Error()
	case errors.Is(err, context.Canceled):
		code, message = http.StatusServiceUnavailable, "login canceled"
	}
	httpjson.Write(w, code, map[string]string{"error": message})
}
