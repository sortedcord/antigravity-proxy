package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/quota"
	quotastatus "antigravity-proxy/internal/status"
)

const quotaTestResponse = `{"groups":[
	{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-weekly","remainingFraction":0.8},{"bucketId":"gemini-5h","remainingFraction":0}]},
	{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-weekly","remainingFraction":0.6},{"bucketId":"3p-5h","remainingFraction":0.4}]}]}`

func TestFetchQuotaSnapshotUsesHubIdentityWithoutProvisioning(t *testing.T) {
	t.Setenv("ANTIGRAVITY_CLIENT_VERSION", "native-test-version")
	for _, project := range []string{"", "configured-project", "cached-project"} {
		t.Run(project, func(t *testing.T) {
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/v1internal:retrieveUserQuotaSummary" {
					t.Errorf("quota attempted other/provisioning request: %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected endpoint", http.StatusBadRequest)
					return
				}
				if r.Header.Get("Authorization") != "Bearer quota-test-token" {
					t.Error("quota did not reuse configured account token")
				}
				if got, want := r.Header.Get("User-Agent"), fmt.Sprintf("antigravity/hub/2.9.1 %s/%s", runtime.GOOS, runtime.GOARCH); got != want {
					t.Errorf("quota user agent = %q, want %q", got, want)
				}
				if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" || r.Header.Get("X-Client-Version") != "native-test-version" {
					t.Error("quota did not retain normal non-UA transport headers")
				}
				var payload map[string]string
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("quota body: %v", err)
				} else if project == "" && len(payload) != 0 {
					t.Errorf("quota with no project body = %v, want {}", payload)
				} else if project != "" && (len(payload) != 1 || payload["project"] != project) {
					t.Errorf("quota body = %v, want existing project only", payload)
				}
				_, _ = io.WriteString(w, quotaTestResponse)
			}))
			defer upstream.Close()
			cfg := config.Config{AccessToken: "quota-test-token", DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL}
			if project == "configured-project" {
				cfg.ProjectID = project
			}
			p := New(cfg)
			if project == "cached-project" {
				p.projectMu.Lock()
				p.projectID = project
				p.projectMu.Unlock()
			}
			before := time.Now()
			snapshot, err := p.QuotaFetcher().Fetch(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 1 || snapshot.Source != "retrieveUserQuotaSummary" || snapshot.ObservedAt.Before(before) || snapshot.ObservedAt.After(time.Now()) {
				t.Fatalf("observation metadata/requests = %+v / %d", snapshot, requests.Load())
			}
			if *snapshot.Pools.Gemini.FiveHour.RemainingFraction != 0 || *snapshot.Pools.Gemini.Weekly.RemainingFraction != 0.8 || *snapshot.Pools.ThirdParty.FiveHour.RemainingFraction != 0.4 || *snapshot.Pools.ThirdParty.Weekly.RemainingFraction != 0.6 {
				t.Fatalf("fetched pool/window mapping = %+v", snapshot.Pools)
			}
			encoded, _ := json.Marshal(snapshot)
			if strings.Contains(string(encoded), cfg.AccessToken) || strings.Contains(string(encoded), "configured-project") || strings.Contains(string(encoded), "cached-project") {
				t.Fatal("quota snapshot leaked auth/project request data")
			}
		})
	}
}

func TestQuotaIdentityDoesNotChangeNativeTransport(t *testing.T) {
	t.Setenv("ANTIGRAVITY_CLIENT_VERSION", "custom-native-version")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != fmt.Sprintf("antigravity/custom-native-version %s/%s", runtime.GOOS, runtime.GOARCH) || r.Header.Get("X-Client-Version") != "custom-native-version" || r.Header.Get("Accept") != "text/event-stream" {
			t.Error("non-quota native headers changed")
		}
		_, _ = io.WriteString(w, "{}")
	}))
	defer upstream.Close()
	p := New(config.Config{DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
	resp, err := p.postToAntigravity(context.Background(), "native-test-token", "/v1internal:streamGenerateContent", "text/event-stream", "", map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}

func TestFetchQuotaSnapshotReusesTokenCacheAndEndpointFallback(t *testing.T) {
	var dailyCalls, prodCalls atomic.Int32
	daily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dailyCalls.Add(1)
		if r.URL.Path != "/v1internal:retrieveUserQuotaSummary" {
			t.Error("fallback attempted project discovery")
		}
		http.Error(w, "upstream failed", http.StatusServiceUnavailable)
	}))
	defer daily.Close()
	prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prodCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer cached-test-token" || r.URL.Path != "/v1internal:retrieveUserQuotaSummary" {
			t.Error("fallback failed to reuse cached token or quota path")
		}
		_, _ = io.WriteString(w, quotaTestResponse)
	}))
	defer prod.Close()
	p := New(config.Config{RefreshToken: "unused-test-refresh-token", DailyEndpoint: daily.URL, ProdEndpoint: prod.URL})
	p.cachedToken, p.tokenExpiresAt = "cached-test-token", time.Now().Add(time.Hour)
	if _, err := p.QuotaFetcher().Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dailyCalls.Load() != 1 || prodCalls.Load() != 1 {
		t.Fatalf("fallback requests = %d / %d", dailyCalls.Load(), prodCalls.Load())
	}
}

func TestFetchQuotaSnapshotCancelsWithoutFallback(t *testing.T) {
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer upstream.Close()
	var fallbackCalls atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		_, _ = io.WriteString(w, quotaTestResponse)
	}))
	defer fallback.Close()
	p := New(config.Config{AccessToken: "cancel-test-token", DailyEndpoint: upstream.URL, ProdEndpoint: fallback.URL})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := p.QuotaFetcher().Fetch(ctx)
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("quota request did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("quota request ignored context cancellation")
	}
	if fallbackCalls.Load() != 0 {
		t.Fatal("canceled quota request tried fallback")
	}
}

func TestFetchQuotaSnapshotDoesNotWaitForProjectDiscovery(t *testing.T) {
	for _, cancelQuota := range []bool{false, true} {
		name := "completes"
		if cancelQuota {
			name = "cancels"
		}
		t.Run(name, func(t *testing.T) {
			discoveryStarted := make(chan struct{})
			releaseDiscovery := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Drain POST bodies so the server observes client cancellation.
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read request body: %v", err)
					return
				}
				switch r.URL.Path {
				case "/v1internal:loadCodeAssist":
					close(discoveryStarted)
					<-releaseDiscovery
					_, _ = io.WriteString(w, `{"cloudaicompanionProject":"discovered-project"}`)
				case "/v1internal:retrieveUserQuotaSummary":
					if strings.TrimSpace(string(body)) != "{}" {
						t.Errorf("busy discovery quota body = %s, want {}", body)
					}
					if cancelQuota {
						<-r.Context().Done()
						return
					}
					_, _ = io.WriteString(w, quotaTestResponse)
				default:
					t.Errorf("unexpected provisioning request %s", r.URL.Path)
					http.Error(w, "unexpected endpoint", http.StatusBadRequest)
				}
			}))
			defer upstream.Close()
			p := New(config.Config{AccessToken: "busy-discovery-token", DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
			discoveryDone := make(chan struct{})
			go func() {
				defer close(discoveryDone)
				if project, err := p.getProjectID(context.Background(), "busy-discovery-token"); err != nil || project != "discovered-project" {
					t.Errorf("native project discovery = %q, %v", project, err)
				}
			}()
			var quotaDone chan struct{}
			defer func() {
				// Unblock native discovery before waiting or closing the server,
				// including the pre-fix failure where quota waits on its mutex.
				close(releaseDiscovery)
				<-discoveryDone
				if quotaDone != nil {
					<-quotaDone
				}
			}()
			select {
			case <-discoveryStarted:
			case <-time.After(2 * time.Second):
				t.Fatal("native project discovery fixture did not start")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			result := make(chan error, 1)
			quotaDone = make(chan struct{})
			go func() {
				defer close(quotaDone)
				snapshot, err := p.QuotaFetcher().Fetch(ctx)
				if err == nil && snapshot.Pools.Gemini.FiveHour.Status != "available" {
					err = fmt.Errorf("quota returned incorrect snapshot")
				}
				result <- err
			}()
			select {
			case err := <-result:
				if cancelQuota {
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("quota cancellation while discovery blocked = %v", err)
					}
				} else if err != nil {
					t.Fatalf("quota completion while discovery blocked = %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("quota fetch waited for in-flight project discovery")
			}
		})
	}
}

func TestFetchQuotaSnapshotUsesConfiguredProjectWithoutWaitingForCache(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 1 || body["project"] != "configured-project" {
			t.Errorf("configured quota body = %v, error = %v", body, err)
		}
		if r.URL.Path != "/v1internal:retrieveUserQuotaSummary" {
			t.Error("configured quota attempted discovery")
		}
		_, _ = io.WriteString(w, quotaTestResponse)
	}))
	defer upstream.Close()
	p := New(config.Config{AccessToken: "configured-busy-token", ProjectID: "configured-project", DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
	p.projectMu.Lock()
	done := make(chan struct{})
	defer func() {
		p.projectMu.Unlock()
		<-done
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		defer close(done)
		_, err := p.QuotaFetcher().Fetch(ctx)
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("configured quota blocked on cache mutex: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("configured quota waited for project cache mutex")
	}
}

func TestQuotaCollectorCloseDoesNotWaitForUnrelatedTokenRefresh(t *testing.T) {
	// A usable cached token must suffice; any accidental refresh is prevented
	// from reaching Google and would make the recovery assertion fail.
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", "")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", "")
	var quotaCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		quotaCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path != "/v1internal:retrieveUserQuotaSummary" || r.Header.Get("Authorization") != "Bearer freshly-cached-token" {
			t.Error("following quota poll did not use the fresh cached token")
		}
		_, _ = io.WriteString(w, quotaTestResponse)
	}))
	defer upstream.Close()
	p := New(config.Config{RefreshToken: "unrelated-refresh-token", DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
	started := make(chan struct{})
	collector, err := quotastatus.Open(t.TempDir()+"/history.jsonl", time.Hour, func(ctx context.Context) (quota.Snapshot, error) {
		close(started)
		return p.QuotaFetcher().Fetch(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Model a native request that owns tokenMu throughout its OAuth refresh.
	p.tokenMu.Lock()
	locked := true
	defer func() {
		if locked {
			p.tokenMu.Unlock()
		}
		if err := collector.Close(); err != nil {
			t.Errorf("collector cleanup: %v", err)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := collector.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("collector quota fetch did not start")
	}
	cancel()
	closed := make(chan error, 1)
	go func() { closed <- collector.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quota collector Close waited for unrelated token refresh")
	}
	if quotaCalls.Load() != 0 {
		t.Fatal("quota tried its upstream endpoint during unrelated token refresh")
	}
	// Simulate that native refresh completing. The following quota fetch must
	// use this cache without performing an additional OAuth request.
	p.cachedToken, p.tokenExpiresAt = "freshly-cached-token", time.Now().Add(time.Hour)
	p.tokenMu.Unlock()
	locked = false
	snapshot, err := p.QuotaFetcher().Fetch(context.Background())
	if err != nil || snapshot.Pools.Gemini.FiveHour.Status != "available" || quotaCalls.Load() != 1 {
		t.Fatalf("following cached-token quota poll = %+v, error = %v, calls = %d", snapshot, err, quotaCalls.Load())
	}
}

func TestFetchQuotaSnapshotSkipsBusyTokenRefreshAndRecovers(t *testing.T) {
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", "")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", "")
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer direct-fresh-cache" {
			t.Error("quota recovery did not use shared cache")
		}
		_, _ = io.WriteString(w, quotaTestResponse)
	}))
	defer upstream.Close()
	p := New(config.Config{RefreshToken: "busy-refresh-secret", DailyEndpoint: upstream.URL, ProdEndpoint: upstream.URL})
	p.tokenMu.Lock()
	locked := true
	done := make(chan struct{})
	defer func() {
		if locked {
			p.tokenMu.Unlock()
		}
		<-done
	}()
	result := make(chan error, 1)
	go func() {
		defer close(done)
		_, err := p.QuotaFetcher().Fetch(context.Background())
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil || err.Error() != "quota authentication is busy" || strings.Contains(err.Error(), p.cfg.RefreshToken) {
			t.Fatalf("busy quota authentication error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quota fetch waited for unrelated token refresh")
	}
	if calls.Load() != 0 {
		t.Fatal("busy quota authentication issued an upstream request")
	}
	p.cachedToken, p.tokenExpiresAt = "direct-fresh-cache", time.Now().Add(time.Hour)
	p.tokenMu.Unlock()
	locked = false
	if _, err := p.QuotaFetcher().Fetch(context.Background()); err != nil || calls.Load() != 1 {
		t.Fatalf("following quota fetch failed to reuse fresh cache: %v, calls = %d", err, calls.Load())
	}
}

func TestExplicitAccessTokenBypassesOAuthCredentialResolution(t *testing.T) {
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", t.Name()+"-partial-id")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", "")
	cfg := config.Config{AccessToken: t.Name() + "-access", RefreshToken: t.Name() + "-refresh", OAuthClientSecret: t.Name() + "-partial-value"}
	p := New(cfg)
	got, err := p.accessToken(context.Background())
	if err != nil || got != cfg.AccessToken {
		t.Fatal("explicit access token attempted OAuth credential resolution")
	}
}

func TestQuotaAuthenticationSanitizesMissingSavedOAuthPair(t *testing.T) {
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", "")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", "")
	cfg := config.Config{RefreshToken: t.Name() + "-refresh", OAuthClientID: t.Name() + "-partial-id"}
	p := New(cfg)
	_, err := p.QuotaFetcher().Fetch(context.Background())
	if err == nil || strings.Contains(err.Error(), cfg.RefreshToken) || strings.Contains(err.Error(), cfg.OAuthClientID) || strings.Contains(err.Error(), "oauthClient") {
		t.Fatal("quota authentication revealed details of an incomplete saved OAuth pair")
	}
}
