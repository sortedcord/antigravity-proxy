package status

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"antigravity-proxy/internal/quota"
)

// syncFaultFile writes to the real history file and only intercepts Sync, so
// these tests exercise the actual append/truncate bytes rather than an echo store.
type syncFaultFile struct {
	historyFile
	calls      int
	beforeSync func()
	alwaysFail bool
}

func (file *syncFaultFile) Sync() error {
	file.calls++
	if file.beforeSync != nil {
		file.beforeSync()
	}
	if file.calls == 1 || file.alwaysFail {
		return errors.New("injected fsync failure")
	}
	return file.historyFile.Sync()
}

func TestFailedSyncRollsBackBeforePublishingAndCanRecover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history")
	first := observation(observationTime(), 0.8)
	next := observation(observationTime().Add(time.Minute), 0.1)
	current := first
	c := openTestCollector(t, path, time.Second, func(context.Context) (quota.Snapshot, error) { return current, nil })
	c.poll(context.Background())
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	current = next
	file := &syncFaultFile{historyFile: c.file}
	file.beforeSync = func() {
		// appendSample owns the mutex here. The previous observation must still
		// be the published state on both the failed Sync and rollback Sync.
		if !reflect.DeepEqual(c.samples[len(c.samples)-1], first) || !c.polling.LastSuccessAt.Equal(first.ObservedAt) {
			t.Error("snapshot was published before durable Sync")
		}
	}
	c.file = file
	c.poll(context.Background())
	if file.calls != 2 {
		t.Fatalf("Sync calls = %d; want append failure then rollback Sync", file.calls)
	}
	info := requireSnapshot(t, c, first)
	if !strings.Contains(info.LastError, "injected fsync failure") {
		t.Fatalf("Sync error = %q", info.LastError)
	}
	rolledBack, err := os.ReadFile(path)
	if err != nil || string(rolledBack) != string(original) {
		t.Fatalf("failed Sync did not restore exact prior JSONL: %v", err)
	}
	page, err := c.History(Query{})
	if err != nil || !reflect.DeepEqual(page.Entries, allEntries(first)) || page.Total != 4 {
		t.Fatalf("failed Sync history = %+v, %v", page, err)
	}
	file.beforeSync = nil
	c.poll(context.Background())
	if info := requireSnapshot(t, c, next); info.LastError != "" {
		t.Fatalf("recovery error = %q", info.LastError)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestCollector(t, path, time.Second, func(context.Context) (quota.Snapshot, error) { return first, nil })
	requireSnapshot(t, reopened, next)
	page, err = reopened.History(Query{Order: "asc"})
	if err != nil || page.Total != 8 || !reflect.DeepEqual(page.Entries, append(allEntries(first), allEntries(next)...)) {
		t.Fatalf("recovered durable history = %+v, %v", page, err)
	}
}

func TestFailedRollbackSyncPreventsFurtherStorageWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history")
	first := observation(observationTime(), 0.8)
	next := observation(observationTime().Add(time.Minute), 0.1)
	current := first
	c := openTestCollector(t, path, time.Second, func(context.Context) (quota.Snapshot, error) { return current, nil })
	c.poll(context.Background())
	current = next
	file := &syncFaultFile{historyFile: c.file, alwaysFail: true}
	c.file = file
	c.poll(context.Background())
	info := requireSnapshot(t, c, first)
	if !strings.Contains(info.LastError, "rollback failed") {
		t.Fatalf("rollback error = %q", info.LastError)
	}
	priorBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	current = observation(observationTime().Add(2*time.Minute), 0)
	c.poll(context.Background())
	if file.calls != 2 {
		t.Fatalf("faulted storage was reused, Sync calls = %d", file.calls)
	}
	afterBytes, err := os.ReadFile(path)
	if err != nil || string(afterBytes) != string(priorBytes) {
		t.Fatalf("faulted collector appended more data: %v", err)
	}
	if after := requireSnapshot(t, c, first); after.LastError != info.LastError {
		t.Fatalf("durability fault was lost: %+v", after)
	}
}
