package status

import (
	"antigravity-proxy/internal/quota"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestEmptyHistoryAndFailedInitialFetchAreNotFabricated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history")
	entered := make(chan struct{})
	c := openTestCollector(t, path, time.Hour, func(context.Context) (quota.Snapshot, error) {
		close(entered)
		return quota.Snapshot{}, errors.New("no upstream quota")
	})
	page, err := c.History(Query{})
	want := Result{Entries: []Entry{}, Limit: 100, Order: "desc", Pool: "all", Window: "all"}
	if err != nil || !reflect.DeepEqual(page, want) {
		t.Fatalf("initial history = %+v, %v; want %+v", page, err, want)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("initial fetch never entered")
	}
	awaitState(t, c, func(sample quota.Snapshot, exists bool, info PollInfo) bool {
		return !exists && reflect.DeepEqual(sample, quota.Snapshot{}) && info.LastSuccessAt == nil && info.LastAttemptAt != nil && info.LastError == "no upstream quota"
	})
	page, err = c.History(Query{})
	if err != nil || !reflect.DeepEqual(page, want) {
		t.Fatalf("failed initial history = %+v, %v", page, err)
	}
}

func TestAmountOnlyAndUnusableWindowsPreservedWithoutFabricatedPercentages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history")
	sample := observation(observationTime(), 0)
	amount := "-9223372036854775808"
	sample.Buckets[1] = quota.Bucket{ID: "gemini-weekly", PoolDisplayName: "Gemini Models", Window: "weekly", Status: "available", RemainingAmount: &amount}
	sample.Buckets[2] = quota.Bucket{ID: "3p-5h", PoolDisplayName: "Claude and GPT models", Window: "5h", Status: "unavailable", ResetAt: sample.Buckets[2].ResetAt, UnavailableReason: "invalid_remaining_fraction"}
	sample.Buckets[3] = quota.Bucket{ID: "3p-weekly", PoolDisplayName: "Claude and GPT models", Window: "weekly", Status: "disabled", ResetAt: sample.Buckets[3].ResetAt, UnavailableReason: "upstream_disabled"}
	c, err := Open(path, time.Second, 0, func(context.Context) (quota.Snapshot, error) { return sample, nil })
	if err != nil {
		t.Fatal(err)
	}
	c.poll(context.Background())
	requireSnapshot(t, c, sample)
	latest, exists, _ := c.Latest()
	if !exists {
		t.Fatal("latest does not exist")
	}
	if latest.Buckets[0].RemainingFraction == nil || *latest.Buckets[0].RemainingFraction != 0 || latest.Buckets[0].Status != "available" {
		t.Fatal("authoritative zero was lost")
	}
	for _, bucket := range latest.Buckets[1:] {
		if bucket.RemainingFraction != nil {
			t.Fatalf("fabricated fraction for %s: %+v", bucket.ID, bucket)
		}
	}
}
