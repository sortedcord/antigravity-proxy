// Package testutil contains fixtures shared across package boundary tests.
package testutil

import (
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"
)

// UnsetEnv restores each variable with testing's normal cleanup semantics.
func UnsetEnv(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

func IsolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

type RoundTripFunc func(*http.Request) (*http.Response, error)

func (f RoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// RedirectTransport routes fixed provider URLs to a loopback fixture without
// changing process-global transports or the original request.
func RedirectTransport(base http.RoundTripper, target *url.URL) http.RoundTripper {
	return RoundTripFunc(func(request *http.Request) (*http.Response, error) {
		clone := request.Clone(request.Context())
		clone.URL.Scheme, clone.URL.Host = target.Scheme, target.Host
		return base.RoundTrip(clone)
	})
}

func RedirectClient(t *testing.T, address string) *http.Client {
	t.Helper()
	target, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: RedirectTransport(transport, target), Timeout: 5 * time.Second}
}

// Eventually checks an asynchronous invariant with a deadline rather than a
// fixed sleep. Test cleanup still owns cancellation and worker joins.
func Eventually(t *testing.T, condition func() bool, failure string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-timer.C:
			t.Fatal(failure)
		case <-ticker.C:
		}
	}
}

func Await[T any](t *testing.T, signal <-chan T, failure string) T {
	t.Helper()
	select {
	case result := <-signal:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal(failure)
	}
	var zero T
	return zero
}
