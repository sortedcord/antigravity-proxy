package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/terastal-hq/proxylib/accountid"
	"github.com/terastal-hq/proxylib/accounts"
	"github.com/terastal-hq/proxylib/configfile"
)

// ErrUnloadableConfig means an account update was rejected because the resulting
// configuration would fail the same validation Load applies at startup.
var ErrUnloadableConfig = configfile.ErrUnloadableConfig

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

// AccountKey represents an API key owned by an account.
type AccountKey = accounts.Key

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

func (a Account) GetID() string           { return a.ID }
func (a Account) GetUpstreamID() string   { return a.AccountID }
func (a Account) GetKeys() []accounts.Key { return a.Keys }
func (a Account) GetLabel() string        { return a.Label }
func (a Account) GetEmail() string        { return a.AccountEmail }
func (a Account) IsEnabled() bool         { return a.Enabled }
func (a *Account) SetLabel(l string)      { a.Label = l }
func (a *Account) SetEnabled(e bool)      { a.Enabled = e }
func (a *Account) AddKey(k accounts.Key)  { a.Keys = append(a.Keys, k) }
func (a *Account) RemoveKey(keyID string) bool {
	filtered := make([]accounts.Key, 0, len(a.Keys))
	found := false
	for _, k := range a.Keys {
		if k.ID == keyID {
			found = true
			continue
		}
		filtered = append(filtered, k)
	}
	if found {
		a.Keys = filtered
	}
	return found
}

// HasCredentials returns true if the account has an access or refresh token.
func (a Account) HasCredentials() bool {
	return a.AccessToken != "" || a.RefreshToken != ""
}

// ValidateAccountID checks whether id satisfies the account slug requirements:
// 1-32 characters, lowercase alphanumeric and hyphens, starting and ending with alphanumeric.
func ValidateAccountID(id string) error {
	return accountid.Validate(id)
}

// DeriveAccountID generates a unique, valid account slug from an email address.
func DeriveAccountID(email string, existingIDs []string) string {
	return accountid.Derive(email, existingIDs)
}

// ValidateAccounts validates a collection of accounts for uniqueness and key integrity.
func ValidateAccounts(accts []Account) error {
	return accounts.Validate(accts, "Google")
}

var legacyCredentialKeys = []string{
	"accessToken", "refreshToken", "oauthClientId", "oauthClientSecret",
	"accountId", "accountEmail", "accountName", "apiKey",
}

// UpdateAccounts reads the configuration at path, applies update to the accounts,
// validates the result, and durably persists it while stripping legacy flat credential fields.
func UpdateAccounts(path string, update func(accounts *[]Account) error) error {
	var disk Config
	data, err := readConfig(path, &disk)
	if err != nil {
		return err
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

	return configfile.PatchWithData(path, data, func(fields map[string]json.RawMessage) error {
		accountsJSON, err := json.MarshalIndent(accounts, "  ", "  ")
		if err != nil {
			return fmt.Errorf("encode accounts: %w", err)
		}
		fields["accounts"] = accountsJSON
		configfile.StripKeys(fields, legacyCredentialKeys...)
		return nil
	}, func(candidate []byte) error {
		return validateAsLoaded(candidate)
	})
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
