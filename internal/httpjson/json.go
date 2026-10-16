// Package httpjson provides consistent JSON responses for the HTTP API.
package httpjson

import (
	"net/http"

	"github.com/terastal-hq/proxylib/httpx"
)

// Write sends a JSON response without HTML-escaping copied URLs.
func Write(w http.ResponseWriter, status int, value any) {
	httpx.WriteJSON(w, status, value)
}
