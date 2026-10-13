package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFetchQuotaSnapshotErrorsDoNotExposeCredentials(t *testing.T) {
	const secret = "do-not-expose-test-credential"
	for _, test := range []struct {
		name string
		code int
		body string
	}{
		{"upstream error", http.StatusUnauthorized, secret},
		{"malformed response", http.StatusOK, `{"` + secret + `":`},
		{"wrong schema", http.StatusOK, `{"groups":"` + secret + `"}`},
		{"oversized response", http.StatusOK, strings.Repeat("x", (8<<20)+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.code)
				_, _ = io.WriteString(w, test.body)
			}))
			defer upstream.Close()
			fetcher := loopbackFetcher(upstream.URL, secret)
			snapshot, err := fetcher.Fetch(context.Background())
			if err == nil || !snapshot.ObservedAt.IsZero() {
				t.Fatal("failed source produced a successful observation")
			}
			for _, credential := range []string{secret, "do-not-expose-refresh", "do-not-expose-local-key"} {
				if strings.Contains(err.Error(), credential) {
					t.Fatal("quota polling error exposed a credential")
				}
			}
		})
	}
	fetcher := NewFetcher(Transport{
		AccessToken: func(context.Context) (string, error) { return "", errors.New("missing credentials") },
	})
	if _, err := fetcher.Fetch(context.Background()); err == nil {
		t.Fatal("quota source accepted missing credentials")
	}
}

func TestFetchQuotaSnapshotObservesAvailableDisabledMissingAndRecovery(t *testing.T) {
	var response atomic.Value
	response.Store(`{"buckets":[{"bucketId":"3p-weekly","remainingFraction":0}]}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, response.Load().(string))
	}))
	defer upstream.Close()
	fetcher := loopbackFetcher(upstream.URL, "state-test-token")
	first, err := fetcher.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		body   string
		status WindowStatus
	}{
		{`{"buckets":[{"bucketId":"3p-weekly","disabled":true}]}`, StatusDisabled},
		{`{"buckets":[]}`, ""},
		{`{"buckets":[{"bucketId":"3p-weekly","remainingFraction":"bad"}]}`, StatusUnavailable},
		{`{"buckets":[{"bucketId":"3p-weekly","remainingFraction":1}]}`, StatusAvailable},
	} {
		response.Store(test.body)
		snapshot, err := fetcher.Fetch(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		b, found := snapshot.Bucket("3p-weekly")
		if test.status == "" {
			if found {
				t.Fatalf("expected 3p-weekly not found, got %+v", b)
			}
		} else {
			if !found || b.Status != test.status {
				t.Fatalf("state = %+v, want status %v", b, test.status)
			}
		}
		firstB, _ := first.Bucket("3p-weekly")
		if *firstB.RemainingFraction != 0 || firstB.Status != StatusAvailable {
			t.Fatal("subsequent fetch mutated earlier quota observation")
		}
	}
}

type fetchHTTPError int

func (e fetchHTTPError) Error() string   { return "credential-bearing upstream response" }
func (e fetchHTTPError) HTTPStatus() int { return int(e) }

func loopbackFetcher(endpoint, token string) *Fetcher {
	return NewFetcher(Transport{
		AccessToken: func(context.Context) (string, error) { return token, nil },
		ProjectID:   func() string { return "" },
		Post: func(ctx context.Context, token, path, accept string, payload any) (*http.Response, error) {
			body, err := json.Marshal(payload)
			if err != nil {
				return nil, err
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+path, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Accept", accept)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				return nil, err
			}
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				_ = response.Body.Close()
				return nil, fetchHTTPError(response.StatusCode)
			}
			return response, nil
		},
	})
}

func TestFetcherSanitizesTransportFailures(t *testing.T) {
	const secret = "private-account-credential"
	for _, test := range []struct {
		name    string
		authErr error
		postErr error
		want    string
	}{
		{"authentication", errors.New(secret), nil, "authenticate quota request failed"},
		{"busy account", ErrAuthenticationBusy, nil, "quota authentication is busy"},
		{"transport", nil, errors.New(secret), "quota request failed"},
		{"HTTP status", nil, fetchHTTPError(http.StatusUnauthorized), "quota request returned HTTP 401"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fetcher := NewFetcher(Transport{
				AccessToken: func(context.Context) (string, error) { return secret, test.authErr },
				ProjectID:   func() string { return "" },
				Post: func(context.Context, string, string, string, any) (*http.Response, error) {
					if test.authErr != nil {
						t.Fatal("failed authentication issued a request")
					}
					return nil, test.postErr
				},
			})
			snapshot, err := fetcher.Fetch(context.Background())
			if err == nil || err.Error() != test.want || !snapshot.ObservedAt.IsZero() {
				t.Fatalf("failed fetch = %+v, %v; want %q", snapshot, err, test.want)
			}
		})
	}
}

type failedQuotaBody struct{ closed bool }

func (*failedQuotaBody) Read([]byte) (int, error) {
	return 0, errors.New("private-response-credential")
}

func (b *failedQuotaBody) Close() error {
	b.closed = true
	return nil
}

func TestFetcherBoundsErrorsAndClosesBody(t *testing.T) {
	body := &failedQuotaBody{}
	fetcher := NewFetcher(Transport{
		AccessToken: func(context.Context) (string, error) { return "test-token", nil },
		ProjectID:   func() string { return "" },
		Post: func(context.Context, string, string, string, any) (*http.Response, error) {
			return &http.Response{Body: body}, nil
		},
	})
	snapshot, err := fetcher.Fetch(context.Background())
	if err == nil || err.Error() != "read quota response failed" || !snapshot.ObservedAt.IsZero() || !body.closed {
		t.Fatalf("failed body read = %+v, %v, closed=%t", snapshot, err, body.closed)
	}
}

func TestFetcherReturnsCancellationBeforeAuthentication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fetcher := NewFetcher(Transport{
		AccessToken: func(context.Context) (string, error) {
			t.Fatal("canceled fetch attempted authentication")
			return "", nil
		},
	})
	if _, err := fetcher.Fetch(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled fetch error = %v", err)
	}
}
