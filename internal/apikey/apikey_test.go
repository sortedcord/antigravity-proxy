package apikey

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGenerateKeyFormat(t *testing.T) {
	key, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if !strings.HasPrefix(key, Prefix) {
		t.Fatalf("Generate() key = %q, want prefix %q", key, Prefix)
	}
	// Prefix (4) + 43 characters = 47 characters total
	if len(key) != 47 {
		t.Fatalf("Generate() key length = %d, want 47", len(key))
	}
	// Verify characters are strictly base62
	chars := key[len(Prefix):]
	for i, c := range chars {
		if !strings.ContainsRune(base62Chars, c) {
			t.Fatalf("Generate() character at %d = %c is not in base62", i, c)
		}
	}
}

func TestGenerateUniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 50; i++ {
		key, err := Generate()
		if err != nil {
			t.Fatalf("Generate() error = %v", err)
		}
		if seen[key] {
			t.Fatalf("duplicate key generated: %q", key)
		}
		seen[key] = true
	}
}

func TestHashAndMatch(t *testing.T) {
	secret := "agp_test_secret_12345"
	hash := Hash(secret)

	if !strings.HasPrefix(hash, "sha256:") {
		t.Fatalf("Hash(%q) = %q, want prefix 'sha256:'", secret, hash)
	}
	if !Match(secret, hash) {
		t.Fatalf("Match(%q, %q) = false, want true", secret, hash)
	}
	if Match("wrong_secret", hash) {
		t.Fatalf("Match(wrong, hash) = true, want false")
	}
	// Plaintext matching
	if !Match("plain_secret", "plain_secret") {
		t.Fatalf("Match plaintext failed")
	}
	if Match("plain_secret", "other") {
		t.Fatalf("Match plaintext returned true for mismatched strings")
	}
}

func TestLast4(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"agp_12345678", "5678"},
		{"abcd", "abcd"},
		{"ab", "ab"},
		{"", ""},
	}
	for _, tc := range cases {
		got := Last4(tc.input)
		if got != tc.want {
			t.Errorf("Last4(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestExtractPrecedence(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://localhost:8080/v1beta/models?key=query-key", nil)
	req.Header.Set("x-goog-api-key", "goog-key")
	req.Header.Set("x-api-key", "x-key")
	req.Header.Set("Authorization", "Bearer bearer-key")

	// 1. x-goog-api-key has highest priority
	if got := Extract(req, true); got != "goog-key" {
		t.Fatalf("Extract() = %q, want goog-key", got)
	}

	// 2. Query param is next if allowQuery is true
	req.Header.Del("x-goog-api-key")
	if got := Extract(req, true); got != "query-key" {
		t.Fatalf("Extract(allowQuery=true) = %q, want query-key", got)
	}

	// When allowQuery is false, query is ignored
	if got := Extract(req, false); got != "x-key" {
		t.Fatalf("Extract(allowQuery=false) = %q, want x-key", got)
	}

	// 3. x-api-key
	req.URL.RawQuery = ""
	if got := Extract(req, false); got != "x-key" {
		t.Fatalf("Extract() = %q, want x-key", got)
	}

	// 4. Authorization: Bearer
	req.Header.Del("x-api-key")
	if got := Extract(req, false); got != "bearer-key" {
		t.Fatalf("Extract() = %q, want bearer-key", got)
	}

	// Empty
	req.Header.Del("Authorization")
	if got := Extract(req, false); got != "" {
		t.Fatalf("Extract() = %q, want empty", got)
	}
}

func TestRegistryLookup(t *testing.T) {
	key1, _ := Generate()
	key2, _ := Generate()

	hash1 := Hash(key1)
	hash2 := Hash(key2)

	bindings := map[string]Binding{
		hash1: {AccountID: "work", KeyID: "key_1"},
		hash2: {AccountID: "personal", KeyID: "key_2"},
	}

	reg, err := NewRegistry(bindings)
	if err != nil {
		t.Fatalf("NewRegistry error = %v", err)
	}

	b, ok := reg.Lookup(key1)
	if !ok || b.AccountID != "work" || b.KeyID != "key_1" {
		t.Fatalf("reg.Lookup(key1) = (%+v, %v), want (work, key_1, true)", b, ok)
	}

	b, ok = reg.Lookup(key2)
	if !ok || b.AccountID != "personal" || b.KeyID != "key_2" {
		t.Fatalf("reg.Lookup(key2) = (%+v, %v), want (personal, key_2, true)", b, ok)
	}

	_, ok = reg.Lookup("agp_unknown_key")
	if ok {
		t.Fatalf("reg.Lookup(unknown) = true, want false")
	}
}
