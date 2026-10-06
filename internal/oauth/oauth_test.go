package oauth

import "testing"

func TestOAuthClientCredentialsRequiredFromEnvironment(t *testing.T) {
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", "")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", "")
	if _, _, err := oauthClientCredentials(); err == nil {
		t.Fatal("oauthClientCredentials() succeeded without configured credentials")
	}
}

func TestOAuthClientCredentialsTrimEnvironmentValues(t *testing.T) {
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", "  client-id  ")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", "  client-secret  ")
	clientID, clientSecret, err := oauthClientCredentials()
	if err != nil {
		t.Fatalf("oauthClientCredentials() error = %v", err)
	}
	if clientID != "client-id" || clientSecret != "client-secret" {
		t.Fatalf("credentials were not trimmed: id=%q secret=%q", clientID, clientSecret)
	}
}
