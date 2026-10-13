package status

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"antigravity-proxy/internal/quota"
)

func TestReloadedHistoryRemainsAvailableDuringFirstFetch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history")
	stored := observation(observationTime(), 0.6)
	initial, err := Open(path, time.Second, 0, func(context.Context) (quota.Snapshot, error) { return stored, nil })
	if err != nil {
		t.Fatal(err)
	}
	initial.poll(context.Background())
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	c := openTestCollector(t, path, time.Hour, func(ctx context.Context) (quota.Snapshot, error) {
		close(entered)
		<-ctx.Done()
		return quota.Snapshot{}, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("immediate reload poll never entered")
	}
	info := requireSnapshot(t, c, stored)
	if info.LastAttemptAt == nil || info.LastError != "" {
		t.Fatalf("pending loaded polling = %+v", info)
	}
	page, err := c.History(Query{})
	if err != nil || page.Total != 4 || !reflect.DeepEqual(page.Entries, allEntries(stored)) {
		t.Fatalf("pending loaded history = %+v, %v", page, err)
	}
	cancel()
	select {
	case <-c.workerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("parent cancellation did not join worker")
	}
	info = requireSnapshot(t, c, stored)
	if info.LastError != context.Canceled.Error() {
		t.Fatalf("canceled attempt error = %q", info.LastError)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

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

func TestAmountOnlyAndUnusableWindowsPersistWithoutFabricatedPercentages(t *testing.T) {
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
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestCollector(t, path, time.Second, func(context.Context) (quota.Snapshot, error) { return sample, nil })
	requireSnapshot(t, reopened, sample)
	page, err := reopened.History(Query{})
	if err != nil || page.Total != 4 || !reflect.DeepEqual(page.Entries, allEntries(sample)) {
		t.Fatalf("mixed window history = %+v, %v", page, err)
	}
	if page.Entries[0].RemainingFraction == nil || *page.Entries[0].RemainingFraction != 0 || page.Entries[0].Status != "available" {
		t.Fatal("authoritative zero was lost")
	}
	for _, entry := range page.Entries[1:] {
		if entry.RemainingFraction != nil {
			t.Fatalf("fabricated fraction for %s: %+v", entry.ID, entry)
		}
	}
}
