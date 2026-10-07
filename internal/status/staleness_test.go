package status

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/quota"
)

func TestSnapshotStalenessUsesTwoIntervalsWithExactBoundaries(t *testing.T) {
	observed := observationTime()
	interval := 5 * time.Minute
	for _, test := range []struct {
		name string
		now  time.Time
		err  string
		want bool
	}{
		{"future observation", observed.Add(-time.Minute), "", false},
		{"first interval passed", observed.Add(interval + time.Second), "", false},
		{"just before grace", observed.Add(2*interval - time.Nanosecond), "", false},
		{"at grace", observed.Add(2 * interval), "", false},
		{"just after grace", observed.Add(2*interval + time.Nanosecond), "", true},
		{"failed poll is stale immediately", observed, "quota unavailable", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := snapshotStale(observed, interval, test.err, test.now); got != test.want {
				t.Fatalf("stale = %v; want %v", got, test.want)
			}
		})
	}
}

func TestSnapshotStalenessGraceDoesNotOverflowLargeIntervals(t *testing.T) {
	observed := observationTime()
	for _, interval := range []time.Duration{time.Duration(1<<63-1) / 2, time.Duration(1<<63-1)/2 + 1, time.Duration(1<<63 - 1)} {
		boundary := observed.Add(interval).Add(interval)
		if snapshotStale(observed, interval, "", boundary.Add(-time.Nanosecond)) || snapshotStale(observed, interval, "", boundary) {
			t.Fatalf("large interval %s became stale before or at its grace boundary", interval)
		}
		if !snapshotStale(observed, interval, "", boundary.Add(time.Nanosecond)) {
			t.Fatalf("large interval %s did not become stale after its grace boundary", interval)
		}
	}
}

func TestStatusLimitRemainsFreshWhileNextPollCanBeInFlight(t *testing.T) {
	observed := time.Now().UTC().Add(-90 * time.Second)
	sample := observation(observed, 0.5)
	c := openRetainedCollector(t, filepath.Join(t.TempDir(), "history"), 2, func(context.Context) (quota.Snapshot, error) { return sample, nil })
	c.poll(context.Background())
	service := NewService(config.Config{}, nil)
	service.collector = c
	response := httptest.NewRecorder()
	service.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status/limit", nil))
	var body struct {
		Stale bool `json:"stale"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusOK || body.Stale {
		t.Fatalf("observation between one and two intervals = %s (HTTP %d), %v", response.Body.String(), response.Code, err)
	}
}
