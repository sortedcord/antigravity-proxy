// Package httpjson provides consistent JSON responses for the HTTP API.
package httpjson

import (
	"encoding/json"
	"net/http"
)

// Write sends a JSON response without HTML-escaping copied URLs. Callers own
// cache policy and authentication. Encoding failures after headers commit cannot
// safely be replaced with a second response.
func Write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}
