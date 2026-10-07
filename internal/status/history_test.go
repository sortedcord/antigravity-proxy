package status

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"antigravity-proxy/internal/quota"
)

func observation(at time.Time, remaining float64) quota.Snapshot {
	reset := at.Add(5 * time.Hour)
	amount := "12345"
	window := func(id, name string, fraction float64) quota.Window {
		percent, used := fraction*100, (1-fraction)*100
		return quota.Window{BucketID: id, Window: name, Status: "available", RemainingFraction: &fraction, RemainingPercent: &percent, UsedPercent: &used, RemainingAmount: &amount, ResetAt: &reset}
	}
	return quota.Snapshot{ObservedAt: at, Source: "retrieveUserQuotaSummary", Pools: quota.Pools{
		Gemini:     quota.Pool{ID: quota.GeminiPool, DisplayName: "Gemini Models", FiveHour: window("gemini-5h", "5h", remaining), Weekly: window("gemini-weekly", "weekly", 0.75)},
		ThirdParty: quota.Pool{ID: quota.ThirdPartyPool, DisplayName: "Claude and GPT models", FiveHour: window("3p-5h", "5h", 0.5), Weekly: window("3p-weekly", "weekly", 0.25)},
	}}
}

func observationTime() time.Time { return time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC) }

func openTestCollector(t *testing.T, path string, interval time.Duration, fetch FetchFunc) *Collector {
	t.Helper()
	c, err := Open(path, interval, 0, fetch)
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
	return []Entry{
		{ObservedAt: sample.ObservedAt, Pool: "gemini", PoolDisplayName: "Gemini Models", Window: sample.Pools.Gemini.FiveHour},
		{ObservedAt: sample.ObservedAt, Pool: "gemini", PoolDisplayName: "Gemini Models", Window: sample.Pools.Gemini.Weekly},
		{ObservedAt: sample.ObservedAt, Pool: "third_party", PoolDisplayName: "Claude and GPT models", Window: sample.Pools.ThirdParty.FiveHour},
		{ObservedAt: sample.ObservedAt, Pool: "third_party", PoolDisplayName: "Claude and GPT models", Window: sample.Pools.ThirdParty.Weekly},
	}
}

func TestRestartReloadsDurableSnapshotAndPollingTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.jsonl")
	first := observation(observationTime(), 0.8)
	last := observation(observationTime().Add(time.Minute), 0.2)
	calls := 0
	c, err := Open(path, 300*time.Second, 0, func(context.Context) (quota.Snapshot, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return last, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("Open fetched quota")
	}
	if _, exists, info := c.Latest(); exists || info.LastSuccessAt != nil || info.LastAttemptAt != nil || info.IntervalSeconds != 300 {
		t.Fatalf("unexpected initial state: exists=%v info=%+v", exists, info)
	}
	c.poll(context.Background())
	c.poll(context.Background())
	requireSnapshot(t, c, last)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var records []quota.Snapshot
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var record quota.Snapshot
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if !reflect.DeepEqual(records, []quota.Snapshot{first, last}) {
		t.Fatalf("durable records = %#v", records)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0600 || dirInfo.Mode().Perm() != 0700 {
		t.Fatalf("storage modes = %v/%v", fileInfo.Mode().Perm(), dirInfo.Mode().Perm())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestCollector(t, path, 90*time.Second, func(context.Context) (quota.Snapshot, error) {
		t.Error("reopen fetched quota")
		return quota.Snapshot{}, nil
	})
	info := requireSnapshot(t, reopened, last)
	if info.IntervalSeconds != 90 || info.LastAttemptAt != nil || info.LastError != "" {
		t.Fatalf("reloaded metadata = %+v", info)
	}
	page, err := reopened.History(Query{Order: "asc"})
	if err != nil {
		t.Fatal(err)
	}
	want := append(allEntries(first), allEntries(last)...)
	if !reflect.DeepEqual(page.Entries, want) || page.Total != 8 {
		t.Fatalf("reloaded history = %+v", page)
	}
}

func TestHistoryInclusiveFiltersPaginationAndStableOrdering(t *testing.T) {
	t0 := observationTime()
	a, b, d := observation(t0, 0), observation(t0.Add(time.Minute), 0.4), observation(t0.Add(2*time.Minute), 0.9)
	b.Pools.Gemini.Weekly = quota.Window{BucketID: "gemini-weekly", Window: "weekly", Status: "unavailable", UnavailableReason: "not_reported"}
	b.Pools.ThirdParty.Weekly = quota.Window{BucketID: "3p-weekly", Window: "weekly", Status: "disabled", Disabled: true, UnavailableReason: "upstream_disabled"}
	// Persist out of timestamp order. Latest is the most recently persisted
	// success, while history is ordered by observation time.
	samples := []quota.Snapshot{d, a, b}
	index := 0
	c := openTestCollector(t, filepath.Join(t.TempDir(), "history"), time.Minute, func(context.Context) (quota.Snapshot, error) { value := samples[index]; index++; return value, nil })
	for range samples {
		c.poll(context.Background())
	}
	requireSnapshot(t, c, b)
	ascending := append(append(allEntries(a), allEntries(b)...), allEntries(d)...)
	descending := append(append(allEntries(d), allEntries(b)...), allEntries(a)...)
	bTime, dTime := b.ObservedAt, d.ObservedAt
	cases := []struct {
		name    string
		query   Query
		entries []Entry
		total   int
		next    *int
	}{
		{"default descending", Query{}, descending, 12, nil},
		{"ascending", Query{Order: "asc"}, ascending, 12, nil},
		{"inclusive boundaries", Query{From: &bTime, To: &dTime, Order: "asc"}, ascending[4:], 8, nil},
		{"single time", Query{From: &bTime, To: &bTime}, allEntries(b), 4, nil},
		{"page across observations", Query{Order: "desc", Offset: 3, Limit: 3}, descending[3:6], 12, intPointer(6)},
		{"final partial page", Query{Order: "asc", Offset: 10, Limit: 3}, ascending[10:], 12, nil},
		{"offset past end", Query{Offset: 1000}, []Entry{}, 12, nil},
		{"pool and window", Query{Pool: "third_party", Window: "weekly", Order: "asc"}, []Entry{ascending[3], ascending[7], ascending[11]}, 3, nil},
		{"zero is available", Query{From: &t0, To: &t0, Pool: "gemini", Window: "5h"}, []Entry{ascending[0]}, 1, nil},
		{"unavailable weekly", Query{From: &bTime, To: &bTime, Pool: "gemini", Window: "weekly"}, []Entry{ascending[5]}, 1, nil},
		{"disabled weekly", Query{From: &bTime, To: &bTime, Pool: "third_party", Window: "weekly"}, []Entry{ascending[7]}, 1, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.History(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Entries, tc.entries) || got.Total != tc.total || !reflect.DeepEqual(got.NextOffset, tc.next) {
				t.Fatalf("history = %+v; want entries %#v total %d next %v", got, tc.entries, tc.total, tc.next)
			}
		})
	}
	between := t0.Add(30 * time.Second)
	page, err := c.History(Query{From: &between, To: &between})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 || page.Entries == nil || len(page.Entries) != 0 || page.NextOffset != nil {
		t.Fatalf("empty selection = %+v", page)
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"entries":[]`) || !strings.Contains(string(encoded), `"next_offset":null`) {
		t.Fatalf("empty JSON = %s", encoded)
	}
}

func intPointer(n int) *int { return &n }

func TestHistoryEqualTimestampTieOrder(t *testing.T) {
	a, b := observation(observationTime(), 0.1), observation(observationTime(), 0.2)
	index := 0
	c := openTestCollector(t, filepath.Join(t.TempDir(), "history"), time.Second, func(context.Context) (quota.Snapshot, error) {
		index++
		if index == 1 {
			return a, nil
		}
		return b, nil
	})
	c.poll(context.Background())
	c.poll(context.Background())
	for _, test := range []struct {
		order string
		want  []Entry
	}{{"asc", append(allEntries(a), allEntries(b)...)}, {"desc", append(allEntries(b), allEntries(a)...)}} {
		got, err := c.History(Query{Order: test.order})
		if err != nil || !reflect.DeepEqual(got.Entries, test.want) {
			t.Fatalf("%s ties = %+v, %v", test.order, got, err)
		}
	}
}

func TestHistoryInvalidQueriesAndBoundedOutput(t *testing.T) {
	c := openTestCollector(t, filepath.Join(t.TempDir(), "history"), time.Second, func(context.Context) (quota.Snapshot, error) { return observation(observationTime(), 1), nil })
	from, to := observationTime().Add(time.Second), observationTime()
	for _, query := range []Query{{Pool: "model"}, {Window: "daily"}, {Limit: -1}, {Limit: 1001}, {Offset: -1}, {Order: "newest"}, {From: &from, To: &to}} {
		if _, err := c.History(query); err == nil {
			t.Errorf("accepted invalid query %+v", query)
		}
	}
	for range 275 {
		c.poll(context.Background())
	}
	page, err := c.History(Query{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1000 || page.Total != 1100 || page.NextOffset == nil || *page.NextOffset != 1000 {
		t.Fatalf("bounded page = length %d total %d next %v", len(page.Entries), page.Total, page.NextOffset)
	}
	// Integer-max offsets must not overflow when calculating a page endpoint.
	page, err = c.History(Query{Offset: int(^uint(0) >> 1), Limit: 1000})
	if err != nil || page.Total != 1100 || len(page.Entries) != 0 || page.NextOffset != nil {
		t.Fatalf("large offset = %+v, %v", page, err)
	}
}

func TestSnapshotsAndQueryResultsAreIndependent(t *testing.T) {
	original := observation(observationTime(), 0.4)
	want := cloneSnapshot(original)
	c := openTestCollector(t, filepath.Join(t.TempDir(), "history"), time.Second, func(context.Context) (quota.Snapshot, error) { return original, nil })
	c.poll(context.Background())
	*original.Pools.Gemini.FiveHour.RemainingFraction = 0.9
	*original.Pools.Gemini.FiveHour.ResetAt = observationTime().Add(time.Hour)
	*original.Pools.Gemini.FiveHour.RemainingAmount = "999"
	got, _, info := c.Latest()
	*got.Pools.Gemini.FiveHour.RemainingPercent = 99
	*info.LastSuccessAt = observationTime().Add(time.Hour)
	*info.LastAttemptAt = observationTime().Add(time.Hour)
	from := observationTime()
	page, err := c.History(Query{From: &from})
	if err != nil {
		t.Fatal(err)
	}
	*page.Entries[0].UsedPercent = 99
	*page.Entries[0].ResetAt = observationTime().Add(time.Hour)
	*page.Entries[0].RemainingAmount = "888"
	*page.From = observationTime().Add(time.Hour)
	if !from.Equal(observationTime()) {
		t.Fatal("History mutated caller's filter")
	}
	requireSnapshot(t, c, want)
	next, err := c.History(Query{})
	if err != nil || !reflect.DeepEqual(next.Entries, allEntries(want)) {
		t.Fatalf("mutated cache: %+v, %v", next, err)
	}
}

func TestOpenRejectsInvalidStorageAndIntervals(t *testing.T) {
	dir := t.TempDir()
	fetch := func(context.Context) (quota.Snapshot, error) { return observation(observationTime(), 1), nil }
	for _, test := range []struct {
		name, path string
		interval   time.Duration
		fetch      FetchFunc
	}{
		{"empty path", "", time.Second, fetch}, {"blank path", "  ", time.Second, fetch}, {"zero interval", filepath.Join(dir, "zero"), 0, fetch}, {"negative interval", filepath.Join(dir, "negative"), -time.Second, fetch}, {"nil fetch", filepath.Join(dir, "nil"), time.Second, nil}, {"directory file", dir, time.Second, fetch},
	} {
		t.Run(test.name, func(t *testing.T) {
			if c, err := Open(test.path, test.interval, 0, test.fetch); err == nil {
				c.Close()
				t.Fatal("Open unexpectedly succeeded")
			}
		})
	}
	blocked := filepath.Join(dir, "file")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if c, err := Open(filepath.Join(blocked, "usage"), time.Second, 0, fetch); err == nil {
		c.Close()
		t.Fatal("Open accepted non-directory parent")
	}
	valid, err := json.Marshal(observation(observationTime(), 1))
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"{broken}\n", "{}\n", string(valid) + "\n\n", "{broken}\n" + string(valid)} {
		path := filepath.Join(t.TempDir(), "corrupt")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if c, err := Open(path, time.Second, 0, fetch); err == nil {
			c.Close()
			t.Fatalf("Open accepted corrupt history %q", content)
		}
	}
}

func TestInvalidObservationsDoNotAppend(t *testing.T) {
	good := observation(observationTime(), 0.3)
	cases := []struct {
		name   string
		mutate func(*quota.Snapshot)
	}{
		{"zero time", func(s *quota.Snapshot) { s.ObservedAt = time.Time{} }},
		{"missing source", func(s *quota.Snapshot) { s.Source = "" }},
		{"wrong pool", func(s *quota.Snapshot) { s.Pools.Gemini.ID = "model" }},
		{"wrong window", func(s *quota.Snapshot) { s.Pools.Gemini.Weekly.Window = "daily" }},
		{"wrong bucket", func(s *quota.Snapshot) { s.Pools.Gemini.Weekly.BucketID = "unknown" }},
		{"unknown status", func(s *quota.Snapshot) { s.Pools.Gemini.Weekly.Status = "full" }},
		{"nan", func(s *quota.Snapshot) { *s.Pools.Gemini.FiveHour.RemainingFraction = math.NaN() }},
		{"infinity", func(s *quota.Snapshot) { *s.Pools.Gemini.FiveHour.UsedPercent = math.Inf(1) }},
		{"range", func(s *quota.Snapshot) { *s.Pools.Gemini.FiveHour.RemainingPercent = 101 }},
		{"disabled contradiction", func(s *quota.Snapshot) { s.Pools.Gemini.Weekly.Disabled = true }},
		{"unavailable percentages", func(s *quota.Snapshot) { s.Pools.Gemini.Weekly.Status = "unavailable" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			current := cloneSnapshot(good)
			path := filepath.Join(t.TempDir(), "history")
			c := openTestCollector(t, path, time.Second, func(context.Context) (quota.Snapshot, error) { return current, nil })
			c.poll(context.Background())
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			current = cloneSnapshot(good)
			tc.mutate(&current)
			c.poll(context.Background())
			if info := requireSnapshot(t, c, good); info.LastError == "" {
				t.Fatal("invalid observation has no LastError")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatalf("invalid observation changed history: %v", err)
			}
		})
	}
}
