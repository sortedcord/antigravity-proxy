package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"antigravity-proxy/internal/apikey"
)

// ErrUnloadableConfig means an account update was rejected because the resulting
// configuration would fail the same validation Load applies at startup.
var ErrUnloadableConfig = errors.New("change rejected: the resulting configuration would not load at startup")

// ApplyCredentials updates an account's token, OAuth client, and Google account identity fields.
func (acc *Account) ApplyCredentials(creds Credentials) {
	acc.AccessToken = creds.AccessToken
	acc.RefreshToken = creds.RefreshToken
	acc.OAuthClientID = creds.OAuthClientID
	acc.OAuthClientSecret = creds.OAuthClientSecret
	acc.AccountID = creds.AccountID
	acc.AccountEmail = creds.AccountEmail
	acc.AccountName = creds.AccountName
}

var accountIDRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$|^[a-z0-9]$`)

// AccountKey represents an API key owned by an account.
type AccountKey struct {
	ID        string    `json:"id"`
	Label     string    `json:"label,omitempty"`
	Hash      string    `json:"hash"`
	Last4     string    `json:"last4"`
	CreatedAt time.Time `json:"createdAt"`
}

// Account holds the configuration, Google OAuth credentials, and API keys for one account.
type Account struct {
	ID                string       `json:"id"`
	Label             string       `json:"label,omitempty"`
	Enabled           bool         `json:"enabled"`
	RefreshToken      string       `json:"refreshToken,omitempty"`
	AccessToken       string       `json:"accessToken,omitempty"`
	OAuthClientID     string       `json:"oauthClientId,omitempty"`
	OAuthClientSecret string       `json:"oauthClientSecret,omitempty"`
	AccountID         string       `json:"accountId,omitempty"`
	AccountEmail      string       `json:"accountEmail,omitempty"`
	AccountName       string       `json:"accountName,omitempty"`
	ProjectID         string       `json:"projectId,omitempty"`
	Keys              []AccountKey `json:"keys,omitempty"`
}

// HasCredentials returns true if the account has an access or refresh token.
func (a Account) HasCredentials() bool {
	return a.AccessToken != "" || a.RefreshToken != ""
}

// ValidateAccountID checks whether id satisfies the account slug requirements:
// 1-32 characters, lowercase alphanumeric and hyphens, starting and ending with alphanumeric.
func ValidateAccountID(id string) error {
	if len(id) == 0 {
		return fmt.Errorf("account ID must not be empty")
	}
	if len(id) > 32 {
		return fmt.Errorf("account ID %q exceeds maximum length of 32 characters", id)
	}
	if !accountIDRegex.MatchString(id) {
		return fmt.Errorf("account ID %q must contain only lowercase letters, numbers, and internal hyphens", id)
	}
	return nil
}

// DeriveAccountID generates a unique, valid account slug from an email address.
func DeriveAccountID(email string, existingIDs []string) string {
	parts := strings.Split(email, "@")
	raw := strings.ToLower(strings.TrimSpace(parts[0]))

	var sb strings.Builder
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		} else {
			sb.WriteRune('-')
		}
	}
	slug := strings.Trim(sb.String(), "-")
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	if slug == "" {
		slug = "account"
	}
	if len(slug) > 28 {
		slug = strings.TrimRight(slug[:28], "-")
	}

	candidate := slug
	counter := 2
	for slices.Contains(existingIDs, candidate) {
		suffix := "-" + strconv.Itoa(counter)
		if len(slug)+len(suffix) > 32 {
			candidate = strings.TrimRight(slug[:32-len(suffix)], "-") + suffix
		} else {
			candidate = slug + suffix
		}
		counter++
	}
	return candidate
}

// ValidateAccounts validates a collection of accounts for uniqueness and key integrity.
func ValidateAccounts(accounts []Account) error {
	seenIDs := make(map[string]bool, len(accounts))
	seenGoogleIDs := make(map[string]string, len(accounts))
	seenKeyHashes := make(map[string]string)

	for _, acc := range accounts {
		if err := ValidateAccountID(acc.ID); err != nil {
			return err
		}
		if seenIDs[acc.ID] {
			return fmt.Errorf("duplicate account ID %q", acc.ID)
		}
		seenIDs[acc.ID] = true

		if acc.AccountID != "" {
			if existingAcc, exists := seenGoogleIDs[acc.AccountID]; exists {
				return fmt.Errorf("Google account %q is linked to both %q and %q", acc.AccountID, existingAcc, acc.ID)
			}
			seenGoogleIDs[acc.AccountID] = acc.ID
		}

		keyIDs := make(map[string]bool, len(acc.Keys))
		for _, key := range acc.Keys {
			if key.ID == "" {
				return fmt.Errorf("account %q has key with empty ID", acc.ID)
			}
			if keyIDs[key.ID] {
				return fmt.Errorf("account %q has duplicate key ID %q", acc.ID, key.ID)
			}
			keyIDs[key.ID] = true

			if !strings.HasPrefix(key.Hash, apikey.HashPrefix) {
				return fmt.Errorf("account %q key %q has invalid hash format", acc.ID, key.ID)
			}
			if existingAcc, exists := seenKeyHashes[key.Hash]; exists {
				return fmt.Errorf("duplicate API key hash across accounts %q and %q", existingAcc, acc.ID)
			}
			seenKeyHashes[key.Hash] = acc.ID
		}
	}
	return nil
}

// UpdateAccounts reads the configuration at path, applies update to the accounts,
// validates the result, and durably persists it while stripping legacy flat credential fields.
func UpdateAccounts(path string, update func(accounts *[]Account) error) error {
	var disk Config
	data, err := readConfig(path, &disk)
	if err != nil {
		return err
	}

	fields := make(map[string]json.RawMessage)
	if len(data) != 0 {
		if err := json.Unmarshal(data, &fields); err != nil {
			return fmt.Errorf("read saved accounts: %w", err)
		}
	}

	accounts := disk.Accounts
	if len(accounts) == 0 && disk.hasLegacyCredentials() {
		accounts = disk.migratedLegacyAccounts()
	}

	if err := update(&accounts); err != nil {
		return err
	}

	if err := ValidateAccounts(accounts); err != nil {
		return fmt.Errorf("validate accounts: %w", err)
	}

	accountsJSON, err := json.MarshalIndent(accounts, "  ", "  ")
	if err != nil {
		return fmt.Errorf("encode accounts: %w", err)
	}
	fields["accounts"] = accountsJSON

	// Strip legacy credential fields from disk on update
	legacyKeys := []string{
		"accessToken", "refreshToken", "oauthClientId", "oauthClientSecret",
		"accountId", "accountEmail", "accountName", "apiKey",
	}
	for _, lk := range legacyKeys {
		for key := range fields {
			if strings.EqualFold(key, lk) {
				delete(fields, key)
			}
		}
	}

	outData, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := validateAsLoaded(outData); err != nil {
		return fmt.Errorf("%w: %v", ErrUnloadableConfig, err)
	}

	return saveConfigBytes(path, outData)
}

// validateAsLoaded checks data exactly as Load would at the next startup:
// defaults, then the file, then the environment, then normalization and validation.
func validateAsLoaded(data []byte) error {
	home, _ := os.UserHomeDir()
	cfg := defaults(home)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return err
	}
	if err := cfg.applyEnvironment(); err != nil {
		return err
	}
	cfg.normalize()
	return cfg.Validate()
}
