package status

import (
	"antigravity-proxy/internal/quota"
	"reflect"
	"testing"
	"time"
)

func observation(at time.Time, remaining float64) quota.Snapshot {
	reset := at.Add(5 * time.Hour)
	amount := "12345"
	bucket := func(id, poolName, window string, fraction float64) quota.Bucket {
		return quota.Bucket{
			ID:                id,
			PoolDisplayName:   poolName,
			Window:            window,
			Status:            "available",
			RemainingFraction: &fraction,
			RemainingAmount:   &amount,
			ResetAt:           &reset,
		}
	}
	return quota.Snapshot{
		ObservedAt: at,
		Source:     "retrieveUserQuotaSummary",
		Buckets: []quota.Bucket{
			bucket("gemini-5h", "Gemini Models", "5h", remaining),
			bucket("gemini-weekly", "Gemini Models", "weekly", 0.75),
			bucket("3p-5h", "Claude and GPT models", "5h", 0.5),
			bucket("3p-weekly", "Claude and GPT models", "weekly", 0.25),
		},
	}
}

func observationTime() time.Time { return time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC) }

func openTestCollector(t *testing.T, path string, interval time.Duration, fetch FetchFunc) *Collector {
	t.Helper()
	c, err := Open(path, interval, 0, fetch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.Close()
	})
	return c
}

func openRetainedCollector(t *testing.T, path string, maxSamples int, fetch FetchFunc) *Collector {
	t.Helper()
	c, err := Open(path, time.Minute, maxSamples, fetch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func requireSnapshot(t *testing.T, c *Collector, want quota.Snapshot) PollInfo {
	t.Helper()
	got, exists, info := c.Latest()
	if !exists || !reflect.DeepEqual(got, want) {
		t.Fatalf("latest = %#v, exists %v; want %#v", got, exists, want)
	}
	if info.LastSuccessAt == nil || !info.LastSuccessAt.Equal(want.ObservedAt) {
		t.Fatalf("last success = %v; want %v", info.LastSuccessAt, want.ObservedAt)
	}
	return info
}

func allEntries(sample quota.Snapshot) []Entry {
	entries := make([]Entry, len(sample.Buckets))
	for i, b := range sample.Buckets {
		entries[i] = Entry{ObservedAt: sample.ObservedAt, Bucket: b.Clone()}
	}
	return entries
}
