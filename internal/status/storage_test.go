package status

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/quota"
)

func openRetainedCollector(t *testing.T, path string, maxSamples int, fetch FetchFunc) *Collector {
	t.Helper()
	c, err := Open(path, time.Minute, maxSamples, fetch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("close collector: %v", err)
		}
	})
	return c
}

func encodeHistory(t *testing.T, samples []quota.Snapshot) []byte {
	t.Helper()
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	for _, sample := range samples {
		if err := encoder.Encode(sample); err != nil {
			t.Fatal(err)
		}
	}
	return data.Bytes()
}

func storedHistory(t *testing.T, path string) []quota.Snapshot {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 && data[len(data)-1] != '\n' {
		t.Fatal("stored history has an incomplete tail")
	}
	var samples []quota.Snapshot
	reader := bufio.NewScanner(bytes.NewReader(data))
	for reader.Scan() {
		var sample quota.Snapshot
		if err := json.Unmarshal(reader.Bytes(), &sample); err != nil {
			t.Fatal(err)
		}
		samples = append(samples, sample)
	}
	if err := reader.Err(); err != nil {
		t.Fatal(err)
	}
	return samples
}

func requireRetainedHistory(t *testing.T, c *Collector, path string, samples []quota.Snapshot) {
	t.Helper()
	if got := storedHistory(t, path); !reflect.DeepEqual(got, samples) {
		t.Fatalf("stored append order = %#v; want %#v", got, samples)
	}
	requireSnapshot(t, c, samples[len(samples)-1])
	ordered := append([]quota.Snapshot(nil), samples...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ObservedAt.Before(ordered[j].ObservedAt) })
	for _, order := range []string{"asc", "desc"} {
		var entries []Entry
		for index := range ordered {
			if order == "desc" {
				index = len(ordered) - index - 1
			}
			entries = append(entries, allEntries(ordered[index])...)
		}
		page, err := c.History(Query{Order: order})
		if err != nil || page.Total != len(entries) || !reflect.DeepEqual(page.Entries, entries) {
			t.Fatalf("%s retained history = %+v, %v; want %#v", order, page, err, entries)
		}
		page, err = c.History(Query{Order: order, Offset: 1, Limit: 2})
		if err != nil || page.Total != len(entries) || !reflect.DeepEqual(page.Entries, entries[1:3]) || page.NextOffset == nil || *page.NextOffset != 3 {
			t.Fatalf("%s retained page = %+v, %v", order, page, err)
		}
	}
	at := ordered[0].ObservedAt
	page, err := c.History(Query{From: &at, To: &at, Pool: "third_party", Window: "weekly", Order: "asc"})
	var want []Entry
	for _, sample := range ordered {
		if sample.ObservedAt.Equal(at) {
			want = append(want, allEntries(sample)[3])
		}
	}
	if err != nil || page.Total != len(want) || !reflect.DeepEqual(page.Entries, want) {
		t.Fatalf("retained inclusive filtered history = %+v, %v; want %#v", page, err, want)
	}
}

func TestHistoryTornTailRecoveryWarnsAndSurvivesAppendAndReopen(t *testing.T) {
	first := observation(observationTime(), 0.8)
	next := observation(observationTime().Add(time.Minute), 0.2)
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(oldLogger)
	for _, test := range []struct {
		name   string
		prefix []quota.Snapshot
		tail   []byte
	}{
		{"partial after record", []quota.Snapshot{first}, []byte(`{"observed_at":"20`)},
		{"valid JSON without newline", []quota.Snapshot{first}, bytes.TrimSuffix(encodeHistory(t, []quota.Snapshot{next}), []byte("\n"))},
		{"partial first record", nil, []byte("{partial")},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history.jsonl")
			prefix := encodeHistory(t, test.prefix)
			data := append(append([]byte(nil), prefix...), test.tail...)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			logs.Reset()
			c := openRetainedCollector(t, path, 2, func(context.Context) (quota.Snapshot, error) { return next, nil })
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, prefix) {
				t.Fatalf("recovered bytes = %q, %v; want %q", got, err, prefix)
			}
			var warning struct {
				Level          string `json:"level"`
				Message        string `json:"msg"`
				Path           string `json:"path"`
				DiscardedBytes int    `json:"discarded_bytes"`
			}
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &warning); err != nil || warning.Level != "WARN" || warning.Message != "recovered incomplete quota history record" || warning.Path != c.path || warning.DiscardedBytes != len(test.tail) {
				t.Fatalf("recovery warning = %s, %v", logs.Bytes(), err)
			}
			if len(test.prefix) != 0 {
				requireSnapshot(t, c, first)
			} else if _, exists, _ := c.Latest(); exists {
				t.Fatal("torn first record fabricated an observation")
			}
			c.poll(context.Background())
			want := append(append([]quota.Snapshot(nil), test.prefix...), next)
			requireRetainedHistory(t, c, path, want)
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			logs.Reset()
			reopened := openRetainedCollector(t, path, 2, func(context.Context) (quota.Snapshot, error) { return next, nil })
			requireRetainedHistory(t, reopened, path, want)
			if logs.Len() != 0 {
				t.Fatalf("already recovered file warned again: %s", logs.Bytes())
			}
		})
	}
}

func TestCompleteMalformedHistoryOutsideRetentionIsFatalAndReleasesLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history")
	sample := observation(observationTime(), 1)
	data := append([]byte("{broken}\n"), encodeHistory(t, []quota.Snapshot{sample, sample, sample})...)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	fetch := func(context.Context) (quota.Snapshot, error) { return sample, nil }
	if c, err := Open(path, time.Minute, 1, fetch); err == nil {
		c.Close()
		t.Fatal("retention discarded a complete malformed record without validating it")
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("malformed history was modified: %v", err)
	}
	if err := os.WriteFile(path, encodeHistory(t, []quota.Snapshot{sample}), 0600); err != nil {
		t.Fatal(err)
	}
	c := openRetainedCollector(t, path, 1, fetch)
	requireRetainedHistory(t, c, path, []quota.Snapshot{sample})
}

func TestHistoryRetentionStartupLiveAndReopenPreserveTimeOrdering(t *testing.T) {
	at := observationTime()
	// Appends deliberately jump backward, forward, and tie in observation time.
	all := []quota.Snapshot{
		observation(at.Add(9*time.Minute), 0.9), observation(at, 0.1),
		observation(at.Add(3*time.Minute), 0.2), observation(at.Add(time.Minute), 0.3),
		observation(at.Add(time.Minute), 0.4), observation(at.Add(-time.Minute), 0.5),
		observation(at.Add(4*time.Minute), 0.6), observation(at.Add(4*time.Minute), 0.7),
		observation(at.Add(-2*time.Minute), 0.8), observation(at.Add(2*time.Minute), 0.9),
	}
	for _, maxSamples := range []int{1, 3} {
		t.Run(fmt.Sprint(maxSamples), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history")
			if err := os.WriteFile(path, encodeHistory(t, all[:5]), 0600); err != nil {
				t.Fatal(err)
			}
			index := 5
			fetch := func(context.Context) (quota.Snapshot, error) { sample := all[index]; index++; return sample, nil }
			c := openRetainedCollector(t, path, maxSamples, fetch)
			requireRetainedHistory(t, c, path, all[5-maxSamples:5])
			for index < len(all) {
				c.poll(context.Background())
				requireRetainedHistory(t, c, path, all[index-maxSamples:index])
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openRetainedCollector(t, path, maxSamples, fetch)
			requireRetainedHistory(t, reopened, path, all[len(all)-maxSamples:])
		})
	}
}

func TestTornTailRecoveryAlsoCompactsOversizedHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history")
	var samples []quota.Snapshot
	for index := range 5 {
		samples = append(samples, observation(observationTime().Add(time.Duration(index)*time.Minute), float64(index)/10))
	}
	data := append(encodeHistory(t, samples), []byte("{partial")...)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	fetch := func(context.Context) (quota.Snapshot, error) { return samples[4], nil }
	c := openRetainedCollector(t, path, 2, fetch)
	requireRetainedHistory(t, c, path, samples[3:])
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openRetainedCollector(t, path, 2, fetch)
	requireRetainedHistory(t, reopened, path, samples[3:])
}

func TestHistoryRetentionDefaultsAndServiceConfiguration(t *testing.T) {
	for _, test := range []struct {
		name       string
		maxSamples int
		retained   int
	}{
		{"default", 0, 10000},
		{"configured", 2, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history")
			// Seed in one write, not thousands of fsync-backed polls. Startup must
			// retain the newest appends, and one real poll crosses the live limit.
			samples := make([]quota.Snapshot, test.retained+2)
			for index := range samples {
				samples[index] = observation(observationTime().Add(time.Duration(index)*time.Minute), 0.5)
			}
			if err := os.WriteFile(path, encodeHistory(t, samples), 0600); err != nil {
				t.Fatal(err)
			}
			observations := make(chan quota.Snapshot, 1)
			fetch := func(ctx context.Context) (quota.Snapshot, error) {
				select {
				case sample := <-observations:
					return sample, nil
				case <-ctx.Done():
					return quota.Snapshot{}, ctx.Err()
				}
			}
			cfg := config.Config{QuotaHistoryPath: path, QuotaHistoryMaxSamples: test.maxSamples}
			service := NewService(cfg, fetch)
			if err := service.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			assertRetained := func(collector *Collector, want []quota.Snapshot) {
				t.Helper()
				if got := storedHistory(t, path); !reflect.DeepEqual(got, want) {
					t.Fatalf("durable retention differs: got %d records; want %d", len(got), len(want))
				}
				for _, order := range []string{"asc", "desc"} {
					index := 0
					if order == "desc" {
						index = len(want) - 1
					}
					page, err := collector.History(Query{Pool: quota.GeminiPool, Window: quota.FiveHourWindow, Order: order, Limit: 1})
					if err != nil || page.Total != test.retained || !reflect.DeepEqual(page.Entries, allEntries(want[index])[:1]) {
						t.Fatalf("%s retention query = %+v, %v", order, page, err)
					}
				}
			}
			collector := service.quotaCollector()
			assertRetained(collector, samples[2:])
			next := observation(samples[len(samples)-1].ObservedAt.Add(time.Minute), 0.2)
			observations <- next
			awaitState(t, collector, func(sample quota.Snapshot, exists bool, info PollInfo) bool {
				return exists && sample.ObservedAt.Equal(next.ObservedAt) && info.LastError == ""
			})
			want := append(append([]quota.Snapshot(nil), samples[3:]...), next)
			assertRetained(collector, want)
			if err := service.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openRetainedCollector(t, path, test.maxSamples, fetch)
			assertRetained(reopened, want)
		})
	}
	if c, err := Open(filepath.Join(t.TempDir(), "history"), time.Minute, -1, func(context.Context) (quota.Snapshot, error) { return quota.Snapshot{}, nil }); err == nil {
		c.Close()
		t.Fatal("negative retention was accepted")
	}
}

// The same test binary acts as an independent process, not a second descriptor
// in the parent's process. These probes run on Windows as well as Unix.
func TestHistoryLockProcessHelper(t *testing.T) {
	path := os.Getenv("ANTIGRAVITY_TEST_HISTORY_LOCK_PATH")
	if path == "" {
		return
	}
	c, err := Open(path, time.Minute, 1, func(context.Context) (quota.Snapshot, error) { panic("lock probe fetched quota") })
	if err != nil {
		if !strings.Contains(err.Error(), "lock quota history") {
			t.Fatal(err)
		}
		fmt.Println("HISTORY_LOCKED")
		return
	}
	defer c.Close()
	fmt.Println("HISTORY_OPENED")
	if os.Getenv("ANTIGRAVITY_TEST_HISTORY_LOCK_HOLD") == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
}

func lockProbe(t *testing.T, path string, wantLocked bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHistoryLockProcessHelper$")
	command.Env = append(os.Environ(), "ANTIGRAVITY_TEST_HISTORY_LOCK_PATH="+path, "ANTIGRAVITY_TEST_HISTORY_LOCK_HOLD=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("lock probe failed: %v; %s", err, output)
	}
	want := "HISTORY_OPENED"
	if wantLocked {
		want = "HISTORY_LOCKED"
	}
	if !strings.Contains(string(output), want) {
		t.Fatalf("lock probe = %q; want %s", output, want)
	}
}

func TestHistoryLockHeldAcrossLoadCompactionAndClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history")
	current := observation(observationTime(), 0.8)
	c := openRetainedCollector(t, path, 1, func(context.Context) (quota.Snapshot, error) { return current, nil })
	// Lock acquisition precedes polling, and failed contenders must not remove it.
	lockProbe(t, path, true)
	lockProbe(t, path, true)
	c.poll(context.Background())
	oldHistory, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	oldLock, err := os.Stat(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	current = observation(observationTime().Add(time.Minute), 0.2)
	c.poll(context.Background())
	requireRetainedHistory(t, c, path, []quota.Snapshot{current})
	newHistory, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	newLock, err := os.Stat(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(oldHistory, newHistory) || !os.SameFile(oldLock, newLock) {
		t.Fatal("compaction must replace history without replacing the companion lock inode")
	}
	lockProbe(t, path, true)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	lockProbe(t, path, false)
}

func TestHistoryLockReleasedWhenOwningProcessExits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHistoryLockProcessHelper$")
	command.Env = append(os.Environ(), "ANTIGRAVITY_TEST_HISTORY_LOCK_PATH="+path, "ANTIGRAVITY_TEST_HISTORY_LOCK_HOLD=1")
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill() }()
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- line
	}()
	select {
	case line := <-ready:
		if line != "HISTORY_OPENED\n" {
			_ = command.Wait()
			t.Fatalf("lock holder did not open history: %q; %s", line, stderr.String())
		}
	case <-ctx.Done():
		_ = command.Wait()
		t.Fatal("lock holder timed out")
	}
	lockProbe(t, path, true)
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	lockProbe(t, path, false)
}

type truncateFaultFile struct{ historyFile }

func (truncateFaultFile) Truncate(int64) error { return errors.New("injected truncate failure") }

func TestTornTailRecoveryRequiresTruncateAndSync(t *testing.T) {
	for _, failure := range []string{"truncate", "sync"} {
		t.Run(failure, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history")
			file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if _, err := file.Write([]byte("good\n{partial")); err != nil {
				t.Fatal(err)
			}
			fault := &syncFaultFile{historyFile: file}
			var storage historyFile = fault
			if failure == "truncate" {
				storage = truncateFaultFile{fault}
			}
			if err := recoverHistoryTail(storage, 5); err == nil || !strings.Contains(err.Error(), "injected") {
				t.Fatalf("recovery accepted %s failure: %v", failure, err)
			}
			want := []byte("good\n")
			if failure == "truncate" {
				want = []byte("good\n{partial")
			}
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
				t.Fatalf("recovery bytes after %s failure = %q, %v; want %q", failure, got, err, want)
			}
		})
	}
}

func TestRetentionFailureBeforeReplacementDoesNotPublishOrLoseOldRecords(t *testing.T) {
	for _, failure := range []string{"sync", "replace"} {
		t.Run(failure, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history")
			first := observation(observationTime(), 0.8)
			next := observation(observationTime().Add(time.Minute), 0.2)
			current := first
			c := openRetainedCollector(t, path, 1, func(context.Context) (quota.Snapshot, error) { return current, nil })
			c.poll(context.Background())
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			current = next
			if failure == "sync" {
				c.storage.syncFile = func(*os.File) error {
					if !reflect.DeepEqual(c.samples, []quota.Snapshot{first}) {
						t.Error("retention published before replacement Sync")
					}
					return errors.New("injected retention sync failure")
				}
			} else {
				c.storage.replace = func(string, string) error { return errors.New("injected retention replacement failure") }
			}
			c.poll(context.Background())
			if info := requireSnapshot(t, c, first); !strings.Contains(info.LastError, "injected") {
				t.Fatalf("retention error = %q", info.LastError)
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
				t.Fatalf("failed retention changed prior durable bytes: %v", err)
			}
			requireRetainedHistory(t, c, path, []quota.Snapshot{first})
			entries, err := os.ReadDir(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".quota-history-") {
					t.Fatalf("failed retention leaked temporary file %s", entry.Name())
				}
			}
			c.storage = defaultHistoryStorage()
			c.poll(context.Background())
			requireRetainedHistory(t, c, path, []quota.Snapshot{next})
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openRetainedCollector(t, path, 1, func(context.Context) (quota.Snapshot, error) { return next, nil })
			requireRetainedHistory(t, reopened, path, []quota.Snapshot{next})
		})
	}
}

func TestRetentionDirectorySyncFailureFaultsStorageWithoutPublishing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history")
	first := observation(observationTime(), 0.8)
	next := observation(observationTime().Add(time.Minute), 0.2)
	current := first
	c := openRetainedCollector(t, path, 1, func(context.Context) (quota.Snapshot, error) { return current, nil })
	c.poll(context.Background())
	current = next
	calls := 0
	c.storage.syncDir = func(string) error {
		calls++
		if !reflect.DeepEqual(c.samples, []quota.Snapshot{first}) {
			t.Error("retention published before directory Sync")
		}
		return errors.New("injected retention directory sync failure")
	}
	c.poll(context.Background())
	info := requireSnapshot(t, c, first)
	if !strings.Contains(info.LastError, "storage fault") || !strings.Contains(info.LastError, "injected") {
		t.Fatalf("retention directory error = %q", info.LastError)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	current = observation(observationTime().Add(2*time.Minute), 0.1)
	c.poll(context.Background())
	if calls != 1 {
		t.Fatalf("faulted storage was reused: directory Sync calls %d", calls)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("faulted retention wrote again: %v", err)
	}
	if after := requireSnapshot(t, c, first); after.LastError != info.LastError {
		t.Fatalf("retention durability fault was lost: %+v", after)
	}
	page, err := c.History(Query{})
	if err != nil || !reflect.DeepEqual(page.Entries, allEntries(first)) {
		t.Fatalf("failed retention published query state: %+v, %v", page, err)
	}
	lockProbe(t, path, true)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openRetainedCollector(t, path, 1, func(context.Context) (quota.Snapshot, error) { return next, nil })
	// The replacement occurred, but the failed Sync prevented claiming it was
	// durable in the running collector. Restart validates whichever complete
	// replacement the filesystem exposes, without accepting torn records.
	requireRetainedHistory(t, reopened, path, []quota.Snapshot{next})
}

func TestPeriodicWorkerBoundsHistoryWhilePollingContinues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history")
	at := observationTime()
	index := 0
	fetch := func(context.Context) (quota.Snapshot, error) {
		index++
		return observation(at.Add(time.Duration(index)*time.Minute), 0.5), nil
	}
	c, err := Open(path, 5*time.Millisecond, 2, fetch)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitState(t, c, func(sample quota.Snapshot, exists bool, info PollInfo) bool {
		return exists && !sample.ObservedAt.Before(at.Add(5*time.Minute)) && info.LastError == ""
	})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	latest, exists, _ := c.Latest()
	if !exists {
		t.Fatal("worker did not retain its final durable observation")
	}
	want := []quota.Snapshot{
		observation(latest.ObservedAt.Add(-time.Minute), 0.5), latest,
	}
	requireRetainedHistory(t, c, path, want)
	reopened := openRetainedCollector(t, path, 2, fetch)
	requireRetainedHistory(t, reopened, path, want)
}

func TestStartupCompactionFailureLeavesOriginalHistoryIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history")
	samples := []quota.Snapshot{
		observation(observationTime(), 0.8),
		observation(observationTime().Add(time.Minute), 0.2),
	}
	original := encodeHistory(t, samples)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := lockHistory(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	file, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	c := &Collector{path: path, tempPrefix: historyTempPrefix(path), file: file, maxSamples: 1, storage: defaultHistoryStorage()}
	c.storage.syncFile = func(*os.File) error { return errors.New("injected startup compaction sync failure") }
	if err := c.loadHistory(file); err == nil || !strings.Contains(err.Error(), "injected startup") {
		t.Fatalf("startup compaction durability failure was ignored: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("failed startup compaction replaced history: %v", err)
	}
}

// Exercise the actual production handle after initial open and after both
// successful and failed replacement. O_APPEND can make Truncate fail on Windows
// even when the same test passes on Unix.
func TestProductionHistoryHandlesCanTruncateAndReopen(t *testing.T) {
	for _, scenario := range []string{"initial open", "startup replacement", "live replacement", "failed replacement"} {
		t.Run(scenario, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history")
			all := []quota.Snapshot{
				observation(observationTime(), 0.8),
				observation(observationTime().Add(time.Minute), 0.5),
				observation(observationTime().Add(2*time.Minute), 0.2),
			}
			seed := all[:2]
			if scenario == "startup replacement" {
				seed = all
			}
			if err := os.WriteFile(path, encodeHistory(t, seed), 0600); err != nil {
				t.Fatal(err)
			}
			fetch := func(context.Context) (quota.Snapshot, error) { return all[2], nil }
			c := openRetainedCollector(t, path, 2, fetch)
			retained := all[:2]
			if scenario == "failed replacement" {
				c.storage.replace = func(string, string) error { return errors.New("injected replacement failure") }
				c.poll(context.Background())
				if info := requireSnapshot(t, c, all[1]); !strings.Contains(info.LastError, "injected replacement") {
					t.Fatalf("replacement failure = %q", info.LastError)
				}
			} else if scenario == "live replacement" {
				c.poll(context.Background())
				retained = all[1:]
			} else if scenario == "startup replacement" {
				retained = all[1:]
			}
			requireRetainedHistory(t, c, path, retained)
			// Keep one complete record using the very handle production opened.
			// A fresh collector must see only that durable prefix and append to EOF.
			prefix := encodeHistory(t, retained[:1])
			if err := recoverHistoryTail(c.file, int64(len(prefix))); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, prefix) {
				t.Fatalf("truncated history = %q, %v; want %q", got, err, prefix)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openRetainedCollector(t, path, 2, fetch)
			requireRetainedHistory(t, reopened, path, retained[:1])
			reopened.poll(context.Background())
			requireRetainedHistory(t, reopened, path, []quota.Snapshot{retained[0], all[2]})
		})
	}
}

func TestSameProcessContenderDoesNotReleaseHistoryLock(t *testing.T) {
	for _, alias := range []bool{false, true} {
		t.Run(fmt.Sprint(alias), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history")
			fetch := func(context.Context) (quota.Snapshot, error) { return observation(observationTime(), 0.5), nil }
			owner := openRetainedCollector(t, path, 1, fetch)
			contenderPath := path
			if alias {
				contenderPath = filepath.Join(filepath.Dir(path), "other-history")
				if err := os.Link(path+".lock", contenderPath+".lock"); err != nil {
					t.Skipf("filesystem does not support companion hard links: %v", err)
				}
			}
			if contender, err := Open(contenderPath, time.Minute, 1, fetch); err == nil {
				contender.Close()
				t.Fatal("same-process contender acquired an owned history lock")
			} else if !strings.Contains(err.Error(), "lock quota history") {
				t.Fatal(err)
			}
			// Closing a duplicate fcntl descriptor would accidentally release the
			// process's kernel lock. An independent process must still be excluded.
			lockProbe(t, path, true)
			lockProbe(t, contenderPath, true)
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
			lockProbe(t, path, false)
			lockProbe(t, contenderPath, false)
		})
	}
}
