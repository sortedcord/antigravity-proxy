package status

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteJSONPreservesLinksAndHeaders(t *testing.T) {
	const link = "https://example.com/?first=1&second=2"
	response := httptest.NewRecorder()
	response.Header().Set("Cache-Control", "no-store")
	writeJSON(response, http.StatusServiceUnavailable, map[string]string{"error": link})
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Content-Type") != "application/json" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("JSON response = %d %v", response.Code, response.Header())
	}
	if !strings.Contains(response.Body.String(), link) {
		t.Fatalf("link was escaped in JSON response: %s", response.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body["error"] != link {
		t.Fatalf("decode response = %v, error = %v", body, err)
	}
}
