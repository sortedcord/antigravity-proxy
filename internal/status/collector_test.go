package status

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-proxy/internal/quota"
)

func awaitState(t *testing.T, c *Collector, check func(quota.Snapshot, bool, PollInfo) bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticks := time.NewTicker(time.Millisecond)
	defer ticks.Stop()
	for {
		sample, exists, info := c.Latest()
		if check(sample, exists, info) {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for collector state: exists=%v snapshot=%+v polling=%+v", exists, sample, info)
		case <-ticks.C:
		}
	}
}

func receiveCall(t *testing.T, calls <-chan chan quota.Snapshot) chan quota.Snapshot {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for quota fetch")
		return nil
	}
}

func TestPollingImmediatePeriodicTransitionsAndCancellation(t *testing.T) {
	calls := make(chan chan quota.Snapshot, 8)
	canceled := make(chan struct{}, 1)
	fetch := func(ctx context.Context) (quota.Snapshot, error) {
		reply := make(chan quota.Snapshot, 1)
		select {
		case calls <- reply:
		case <-ctx.Done():
			return quota.Snapshot{}, ctx.Err()
		}
		select {
		case result := <-reply:
			return result, nil
		case <-ctx.Done():
			canceled <- struct{}{}
			return quota.Snapshot{}, ctx.Err()
		}
	}
	path := filepath.Join(t.TempDir(), "history")
	c := openTestCollector(t, path, 40*time.Millisecond, fetch)
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	immediate := receiveCall(t, calls)
	_, exists, initialInfo := c.Latest()
	if exists || initialInfo.LastAttemptAt == nil || initialInfo.LastSuccessAt != nil {
		t.Fatalf("state before immediate completion: exists=%v info=%+v", exists, initialInfo)
	}
	first := observation(observationTime(), 0.9)
	immediate <- first
	awaitState(t, c, func(s quota.Snapshot, exists bool, info PollInfo) bool {
		return exists && reflect.DeepEqual(s, first) && info.LastSuccessAt != nil && info.LastSuccessAt.Equal(first.ObservedAt)
	})
	requireSnapshot(t, c, first)
	second := observation(observationTime().Add(time.Minute), 0)
	periodic := receiveCall(t, calls)
	secondAttempt := requireSnapshot(t, c, first).LastAttemptAt
	if secondAttempt == nil || !secondAttempt.After(*initialInfo.LastAttemptAt) {
		t.Fatalf("attempt timestamps did not advance: %v then %v", initialInfo.LastAttemptAt, secondAttempt)
	}
	periodic <- second
	awaitState(t, c, func(s quota.Snapshot, exists bool, info PollInfo) bool {
		return exists && reflect.DeepEqual(s, second) && info.LastError == ""
	})
	third := observation(observationTime().Add(2*time.Minute), 1)
	third.Pools.ThirdParty.Weekly = quota.Window{BucketID: "3p-weekly", Window: "weekly", Status: "unavailable", UnavailableReason: "not_reported"}
	receiveCall(t, calls) <- third
	awaitState(t, c, func(s quota.Snapshot, exists bool, _ PollInfo) bool { return exists && reflect.DeepEqual(s, third) })
	page, err := c.History(Query{Order: "asc"})
	if err != nil {
		t.Fatal(err)
	}
	want := append(append(allEntries(first), allEntries(second)...), allEntries(third)...)
	if page.Total != 12 || !reflect.DeepEqual(page.Entries, want) {
		t.Fatalf("polling history = %+v; want %#v", page, want)
	}
	// The next periodic fetch blocks until Close cancels its context. It must
	// not add any extra record or replace the last successful snapshot.
	receiveCall(t, calls)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not cancel active fetch")
	}
	requireSnapshot(t, c, third)
	reloaded := openTestCollector(t, path, time.Second, fetch)
	requireSnapshot(t, reloaded, third)
}

func TestSlowPollingNeverOverlapsAndCloseJoinsWorker(t *testing.T) {
	calls := make(chan chan quota.Snapshot, 8)
	var active, maximum atomic.Int32
	stopped := make(chan struct{}, 1)
	fetch := func(ctx context.Context) (quota.Snapshot, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); old < n && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		reply := make(chan quota.Snapshot, 1)
		select {
		case calls <- reply:
		case <-ctx.Done():
			return quota.Snapshot{}, ctx.Err()
		}
		select {
		case result := <-reply:
			return result, nil
		case <-ctx.Done():
			stopped <- struct{}{}
			return quota.Snapshot{}, ctx.Err()
		}
	}
	c := openTestCollector(t, filepath.Join(t.TempDir(), "history"), 10*time.Millisecond, fetch)
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := receiveCall(t, calls)
	select {
	case <-calls:
		t.Fatal("fetches overlapped while first was blocked")
	case <-time.After(75 * time.Millisecond):
	}
	if active.Load() != 1 || maximum.Load() != 1 {
		t.Fatalf("active/max fetches = %d/%d", active.Load(), maximum.Load())
	}
	first <- observation(observationTime(), 0.6)
	receiveCall(t, calls)
	var closers sync.WaitGroup
	closeErrors := make(chan error, 8)
	for range 8 {
		closers.Add(1)
		go func() { defer closers.Done(); closeErrors <- c.Close() }()
	}
	done := make(chan struct{})
	go func() { closers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("concurrent Close did not join canceled worker")
	}
	close(closeErrors)
	for err := range closeErrors {
		if err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-stopped:
	default:
		t.Fatal("active fetch was not canceled before Close returned")
	}
	if active.Load() != 0 || maximum.Load() != 1 {
		t.Fatalf("active/max after Close = %d/%d", active.Load(), maximum.Load())
	}
	requireSnapshot(t, c, observation(observationTime(), 0.6))
	select {
	case <-calls:
		t.Fatal("unexpected extra fetch after close")
	default:
	}
	if err := c.Start(context.Background()); err == nil {
		t.Fatal("Start succeeded after Close")
	}
}

func TestFetchAndSaveFailuresRetainLastGoodSnapshot(t *testing.T) {
	first := observation(observationTime(), 0.7)
	next := observation(observationTime().Add(time.Minute), 0.1)
	calls := 0
	path := filepath.Join(t.TempDir(), "history")
	c, err := Open(path, time.Second, func(context.Context) (quota.Snapshot, error) {
		calls++
		if calls == 2 {
			return quota.Snapshot{}, errors.New("quota upstream unavailable")
		}
		if calls == 1 {
			return first, nil
		}
		return next, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.poll(context.Background())
	before := requireSnapshot(t, c, first)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c.poll(context.Background())
	failed := requireSnapshot(t, c, first)
	if failed.LastError != "quota upstream unavailable" || failed.LastAttemptAt == nil || failed.LastAttemptAt.Before(*before.LastAttemptAt) {
		t.Fatalf("failed polling metadata = %+v", failed)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(data) {
		t.Fatalf("fetch failure altered storage: %v", err)
	}
	c.poll(context.Background())
	if info := requireSnapshot(t, c, next); info.LastError != "" {
		t.Fatalf("successful recovery retained error: %+v", info)
	}
	// Force a real storage error without needing root-sensitive permission
	// tests or a pretend in-memory persistence backend.
	c.mu.Lock()
	err = c.file.Close()
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	c.poll(context.Background())
	if info := requireSnapshot(t, c, next); !strings.Contains(info.LastError, "quota history") {
		t.Fatalf("save error missing from polling metadata: %+v", info)
	}
	page, err := c.History(Query{Order: "asc"})
	want := append(allEntries(first), allEntries(next)...)
	if err != nil || page.Total != 8 || !reflect.DeepEqual(page.Entries, want) {
		t.Fatalf("failure history = %+v, %v", page, err)
	}
	if err := c.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Close after forced file failure = %v", err)
	}
	if err := c.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("idempotent Close changed error = %v", err)
	}
	reloaded := openTestCollector(t, path, time.Second, func(context.Context) (quota.Snapshot, error) { return first, nil })
	requireSnapshot(t, reloaded, next)
}

func TestStartAndCloseLifecycleGuards(t *testing.T) {
	var calls atomic.Int32
	fetch := func(context.Context) (quota.Snapshot, error) {
		calls.Add(1)
		return observation(observationTime(), 1), nil
	}
	c := openTestCollector(t, filepath.Join(t.TempDir(), "before-start"), time.Second, fetch)
	if err := c.Start(nil); err == nil {
		t.Fatal("Start accepted nil context")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("Close before Start fetched quota")
	}
	if err := c.Start(context.Background()); err == nil {
		t.Fatal("closed collector started")
	}
	other := openTestCollector(t, filepath.Join(t.TempDir(), "canceled"), time.Second, fetch)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := other.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := other.Start(context.Background()); err == nil {
		t.Fatal("duplicate Start succeeded")
	}
	select {
	case <-other.workerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("pre-canceled worker did not exit")
	}
	if calls.Load() != 0 {
		t.Fatal("pre-canceled Start fetched quota")
	}
}

func TestConcurrentReadsDuringPollingAndClose(t *testing.T) {
	var counter atomic.Int64
	c := openTestCollector(t, filepath.Join(t.TempDir(), "history"), time.Second, func(context.Context) (quota.Snapshot, error) {
		n := counter.Add(1)
		return observation(observationTime().Add(time.Duration(n)*time.Second), float64(n)/100), nil
	})
	first := observation(observationTime().Add(time.Second), 0.01)
	c.poll(context.Background())
	stop := make(chan struct{})
	failures := make(chan string, 4)
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				sample, exists, info := c.Latest()
				if !exists || info.LastSuccessAt == nil || !info.LastSuccessAt.Equal(sample.ObservedAt) {
					failures <- "latest snapshot and LastSuccessAt are inconsistent"
					return
				}
				*sample.Pools.Gemini.FiveHour.RemainingPercent = 100
				page, err := c.History(Query{Limit: 1, Order: "asc"})
				if err != nil || len(page.Entries) != 1 || !reflect.DeepEqual(page.Entries[0], allEntries(first)[0]) {
					failures <- "ascending boundary changed under concurrent polling"
					return
				}
				*page.Entries[0].RemainingFraction = 0.99
			}
		}()
	}
	for range 19 {
		c.poll(context.Background())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	close(stop)
	readers.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	last := observation(observationTime().Add(20*time.Second), 0.2)
	requireSnapshot(t, c, last)
	page, err := c.History(Query{Pool: "gemini", Window: "5h", Order: "asc"})
	if err != nil {
		t.Fatal(err)
	}
	want := make([]Entry, 0, 20)
	for n := range 20 {
		want = append(want, allEntries(observation(observationTime().Add(time.Duration(n+1)*time.Second), float64(n+1)/100))[0])
	}
	if page.Total != 20 || !reflect.DeepEqual(page.Entries, want) {
		t.Fatalf("concurrent history = %+v", page)
	}
}
