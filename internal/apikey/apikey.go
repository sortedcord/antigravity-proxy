// Package apikey provides key generation, hashing, extraction, and registry lookup
// for Antigravity Proxy account and administration API keys.
package apikey

import (
	"net/http"
	"strings"

	libapikey "github.com/terastal-hq/proxylib/apikey"
)

const (
	// Prefix is the standard prefix for Antigravity Proxy API keys.
	Prefix = "agp_"
	// HashPrefix identifies SHA-256 hashed keys.
	HashPrefix = libapikey.HashPrefix
	// RandomKeyBytes is the entropy size used for generating new keys.
	RandomKeyBytes = libapikey.RandomKeyBytes
)

const base62Chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Binding associates an API key with its owning account and key metadata.
type Binding = libapikey.Binding

// Registry provides constant-time lookup from raw API keys to their account bindings.
type Registry = libapikey.Registry

// KeyMetadata describes a key without exposing its hash or secret.
type KeyMetadata = libapikey.KeyMetadata

// Generate creates a cryptographically secure API key with the "agp_" prefix
// followed by 43 base62 characters (representing 256 bits of entropy).
func Generate() (string, error) {
	return libapikey.Generate(Prefix)
}

// GenerateID creates a random short identifier for an API key (e.g., "key_3f9a1c2e").
func GenerateID() (string, error) {
	return libapikey.GenerateID()
}

// Hash returns the canonical SHA-256 hash representation of a secret: "sha256:<hex>".
func Hash(secret string) string {
	return libapikey.Hash(secret)
}

// HashBytes computes the raw [32]byte SHA-256 digest of a secret.
func HashBytes(secret string) [32]byte {
	return libapikey.HashBytes(secret)
}

// Last4 returns the trailing 4 characters of a secret for display purposes.
// If the secret is shorter than 4 characters, it returns the full secret.
func Last4(secret string) string {
	return libapikey.Last4(secret)
}

// Match compares a raw secret against an expected hash (or plaintext key) in constant time.
func Match(secret, expected string) bool {
	return libapikey.Match(secret, expected)
}

// NewRegistry constructs an immutable Registry from a list of key hash bindings.
func NewRegistry(bindings map[string]Binding) (*Registry, error) {
	return libapikey.NewRegistry(bindings)
}

// Extract retrieves an API key from an HTTP request in prioritized order:
// 1. x-goog-api-key header
// 2. ?key= query parameter (only if allowQuery is true)
// 3. x-api-key header
// 4. Authorization: Bearer <key> header
//
// Admin endpoints should pass allowQuery=false to prevent secrets appearing in access logs or URLs.
func Extract(r *http.Request, allowQuery bool) string {
	if r == nil {
		return ""
	}
	if key := strings.TrimSpace(r.Header.Get("x-goog-api-key")); key != "" {
		return key
	}
	if allowQuery && r.URL != nil {
		if key := strings.TrimSpace(r.URL.Query().Get("key")); key != "" {
			return key
		}
	}
	if key := strings.TrimSpace(r.Header.Get("x-api-key")); key != "" {
		return key
	}
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); auth != "" {
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			return strings.TrimSpace(auth[len("bearer "):])
		}
	}
	return ""
}
