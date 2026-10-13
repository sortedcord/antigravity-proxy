package oauth

import (
	"context"
	"testing"
)

func TestSessionOptionsAndOneTimeKeyRetrieval(t *testing.T) {
	client := successfulSessionClient(t, testUserInfo())
	mgr := NewSessionManagerWithOptions(client, func(ctx context.Context, opts SessionOptions, tokens Tokens, clientID, clientSecret string, user UserInfo) (AccountConfigResult, error) {
		return AccountConfigResult{
			AccountID: "work",
			KeyID:     "key_test_1",
			KeySecret: "agp_secret_key_123",
		}, nil
	})
	defer mgr.Close()

	info, err := mgr.BeginWithOptions(context.Background(), SessionOptions{
		AccountID: "work",
		IssueKey:  "bifrost",
	})
	if err != nil {
		t.Fatalf("BeginWithOptions error = %v", err)
	}
	if info.ID == "" || info.AuthURL == "" {
		t.Fatalf("invalid session info: %+v", info)
	}

	status, found := mgr.GetSession(info.ID)
	if !found || status.Status != "pending" {
		t.Fatalf("GetSession = (%+v, %v), want pending", status, found)
	}

	// Complete the session
	user, res, err := mgr.CompleteWithSessionID(context.Background(), info.ID, "valid-code", mgr.active.state)
	if err != nil {
		t.Fatalf("CompleteWithSessionID error = %v", err)
	}
	if user.Email != "user@example.com" || res.AccountID != "work" || res.KeySecret != "agp_secret_key_123" {
		t.Fatalf("unexpected completion result: user=%+v, res=%+v", user, res)
	}

	// First GetSession retrieves the key
	status, found = mgr.GetSession(info.ID)
	if !found || status.Status != "completed" {
		t.Fatalf("GetSession = (%+v, %v), want completed", status, found)
	}
	if status.Key == nil || status.Key.Secret != "agp_secret_key_123" {
		t.Fatalf("first GetSession expected key secret, got %+v", status.Key)
	}

	// Second GetSession must have secret erased
	status2, _ := mgr.GetSession(info.ID)
	if status2.Key != nil {
		t.Fatalf("second GetSession expected erased key, got %+v", status2.Key)
	}
}
