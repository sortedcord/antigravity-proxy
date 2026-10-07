// Package status collects authoritative quota observations in bounded, durable
// JSONL storage. A companion lock gives each history path one process owner;
// retention preserves the most recently appended observations.
package status

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/quota"
)

// FetchFunc obtains one authoritative quota observation, respecting cancellation.
type FetchFunc = func(context.Context) (quota.Snapshot, error)

// PollInfo describes attempted and durably successful collection, not a promise
// of when the next poll will finish.
type PollInfo struct {
	IntervalSeconds int64      `json:"interval_seconds"`
	LastAttemptAt   *time.Time `json:"last_attempt_at"`
	LastSuccessAt   *time.Time `json:"last_success_at"`
	LastError       string     `json:"last_error,omitempty"`
}

// historyFile is the storage surface used after loading. Production uses a
// writable, non-append os.File so Windows permits truncation during recovery.
// Appends seek to EOF while the collector owns its exclusive companion lock.
// Tests can inject a failed Sync around that same real file.
type historyFile interface {
	io.Writer
	io.Seeker
	Sync() error
	Truncate(int64) error
	Close() error
}

// Collector owns a history file and at most one nonoverlapping polling worker.
// Its public methods are safe for concurrent use. Returned data is independent
// of collector state, including all optional pointer fields.
type Collector struct {
	mu         sync.RWMutex
	file       historyFile
	path       string
	tempPrefix string
	lock       *historyLock
	maxSamples int
	storage    historyStorage
	interval   time.Duration
	fetch      FetchFunc
	samples    []quota.Snapshot
	// ordered holds sample indices in ascending observation-time order. Equal
	// timestamps retain append order; descending queries reverse that order.
	ordered    []int
	polling    PollInfo
	storeFault error

	started    bool
	closed     bool
	cancel     context.CancelFunc
	workerDone chan struct{}
	closeDone  chan struct{}
	closeErr   error
}

// Open creates owner-only history storage and holds an exclusive companion lock
// until Close. It recovers an incomplete final JSONL record, but any complete
// malformed record remains a startup error. Retention keeps the last maxSamples
// appends, independently of their observation times; zero uses the config default.
// Parent-directory symlinks resolve to one storage/lock identity; the history
// file and its companion lock may not themselves be symlinks. Abandoned private
// compaction files for this history are removed under its lock at startup.
// Open does not start a goroutine or fetch data.
func Open(path string, interval time.Duration, maxSamples int, fetch FetchFunc) (*Collector, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("quota history path is empty")
	}
	if interval <= 0 {
		return nil, errors.New("quota poll interval must be positive")
	}
	if maxSamples == 0 {
		maxSamples = config.DefaultQuotaHistoryMaxSamples
	}
	if maxSamples < 1 {
		return nil, errors.New("quota history max samples must be positive")
	}
	if fetch == nil {
		return nil, errors.New("quota fetch function is nil")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve quota history path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create quota history directory: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("resolve quota history directory: %w", err)
	}
	path = filepath.Join(parent, filepath.Base(path))
	if err := inspectHistoryPath(path); err != nil {
		return nil, err
	}
	lock, err := lockHistory(path + ".lock")
	if err != nil {
		return nil, err
	}
	c := &Collector{
		path: path, tempPrefix: historyTempPrefix(path), lock: lock, maxSamples: maxSamples, storage: defaultHistoryStorage(),
		interval: interval, fetch: fetch, closeDone: make(chan struct{}),
		polling: PollInfo{IntervalSeconds: int64(interval / time.Second)},
	}
	fail := func(err error) (*Collector, error) {
		if c.file != nil {
			_ = c.file.Close()
		}
		_ = lock.Close()
		return nil, err
	}
	if err := inspectHistoryPath(path); err != nil {
		return fail(err)
	}
	if err := c.cleanHistoryTemps(); err != nil {
		return fail(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fail(fmt.Errorf("open quota history: %w", err))
	}
	c.file = file
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return fail(errors.New("quota history must be a regular file"))
	}
	if err := file.Chmod(0600); err != nil {
		return fail(fmt.Errorf("protect quota history: %w", err))
	}
	if err := c.loadHistory(file); err != nil {
		return fail(err)
	}
	c.ordered = make([]int, len(c.samples))
	for i := range c.ordered {
		c.ordered[i] = i
	}
	sort.SliceStable(c.ordered, func(i, j int) bool {
		return c.samples[c.ordered[i]].ObservedAt.Before(c.samples[c.ordered[j]].ObservedAt)
	})
	if len(c.samples) != 0 {
		at := c.samples[len(c.samples)-1].ObservedAt
		c.polling.LastSuccessAt = &at
	}
	return c, nil
}

// Start begins an immediate poll followed by a fixed ticker. Slow fetches run
// sequentially, never in overlapping goroutines. A Collector can start only once.
func (c *Collector) Start(ctx context.Context) error {
	if ctx == nil {
		return errors.New("quota polling context is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("quota collector is closed")
	}
	if c.started {
		return errors.New("quota collector is already started")
	}
	ctx, c.cancel = context.WithCancel(ctx)
	c.started = true
	c.workerDone = make(chan struct{})
	go c.run(ctx)
	return nil
}

func (c *Collector) run(ctx context.Context) {
	defer close(c.workerDone)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	c.poll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.poll(ctx)
		}
	}
}

func (c *Collector) poll(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	attempt := time.Now().UTC()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.polling.LastAttemptAt = &attempt
	c.mu.Unlock()

	sample, err := c.fetch(ctx)
	if err == nil {
		err = validateSnapshot(sample)
	}
	if err == nil {
		sample = cloneSnapshot(sample)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = c.appendSample(sample)
	}
	if err != nil {
		c.polling.LastError = err.Error()
		return
	}
	index := len(c.samples)
	if index == c.maxSamples {
		// replaceHistory committed the retained appends plus this sample. Rebase
		// the ordered index while dropping the oldest append, not the oldest time.
		copy(c.samples, c.samples[1:])
		index--
		c.samples[index] = sample
		n := 0
		for _, old := range c.ordered {
			if old != 0 {
				c.ordered[n] = old - 1
				n++
			}
		}
		c.ordered = c.ordered[:n]
	} else {
		c.samples = append(c.samples, sample)
	}
	position := sort.Search(len(c.ordered), func(i int) bool {
		return c.samples[c.ordered[i]].ObservedAt.After(sample.ObservedAt)
	})
	c.ordered = append(c.ordered, 0)
	copy(c.ordered[position+1:], c.ordered[position:])
	c.ordered[position] = index
	at := sample.ObservedAt
	c.polling.LastSuccessAt = &at
	c.polling.LastError = ""
}

// appendSample runs under mu. The cache is only updated after both append and
// Sync succeed. Rollback prevents a failed write from poisoning a later restart.
func (c *Collector) appendSample(sample quota.Snapshot) error {
	if c.storeFault != nil {
		return c.storeFault
	}
	if len(c.samples) == c.maxSamples {
		return c.replaceHistory(c.samples[1:], &sample)
	}
	record, err := json.Marshal(sample)
	if err != nil {
		return fmt.Errorf("encode quota history: %w", err)
	}
	record = append(record, '\n')
	offset, err := c.file.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("seek quota history: %w", err)
	}
	n, err := c.file.Write(record)
	if err == nil && n != len(record) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = c.file.Sync()
	}
	if err == nil {
		return nil
	}
	rollbackErr := c.file.Truncate(offset)
	if rollbackErr == nil {
		rollbackErr = c.file.Sync()
	}
	if rollbackErr != nil {
		c.storeFault = fmt.Errorf("save quota history: %w; rollback failed: %v", err, rollbackErr)
		return c.storeFault
	}
	return fmt.Errorf("save quota history: %w", err)
}

// PollingInfo returns independent polling metadata without copying quota values.
func (c *Collector) PollingInfo() PollInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pollingInfoLocked()
}

func (c *Collector) pollingInfoLocked() PollInfo {
	info := c.polling
	info.LastAttemptAt = cloneTime(info.LastAttemptAt)
	info.LastSuccessAt = cloneTime(info.LastSuccessAt)
	return info
}

// Latest returns the last durably appended observation, including a sample
// loaded at Open, along with independent polling metadata.
func (c *Collector) Latest() (quota.Snapshot, bool, PollInfo) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	info := c.pollingInfoLocked()
	if len(c.samples) == 0 {
		return quota.Snapshot{}, false, info
	}
	return cloneSnapshot(c.samples[len(c.samples)-1]), true, info
}

// Close cancels and joins the polling worker before closing storage. It is
// idempotent, safe concurrently, and valid even when Start was never called.
func (c *Collector) Close() error {
	c.mu.Lock()
	if c.closed {
		done := c.closeDone
		c.mu.Unlock()
		<-done
		c.mu.RLock()
		err := c.closeErr
		c.mu.RUnlock()
		return err
	}
	c.closed = true
	cancel, workerDone := c.cancel, c.workerDone
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if workerDone != nil {
		<-workerDone
	}
	c.mu.Lock()
	if c.file != nil {
		c.closeErr = c.file.Close()
	}
	c.closeErr = errors.Join(c.closeErr, c.lock.Close())
	close(c.closeDone)
	err := c.closeErr
	c.mu.Unlock()
	return err
}

func validateSnapshot(sample quota.Snapshot) error {
	if sample.ObservedAt.IsZero() || sample.Source == "" {
		return errors.New("quota observation requires an observation time and source")
	}
	for _, item := range []struct {
		pool       quota.Pool
		id, prefix string
	}{
		{sample.Pools.Gemini, quota.GeminiPool, "gemini"},
		{sample.Pools.ThirdParty, quota.ThirdPartyPool, "3p"},
	} {
		if item.pool.ID != item.id {
			return fmt.Errorf("invalid quota pool %q", item.pool.ID)
		}
		for _, spec := range []struct {
			window       quota.Window
			name, bucket string
		}{
			{item.pool.FiveHour, quota.FiveHourWindow, item.prefix + "-5h"},
			{item.pool.Weekly, quota.WeeklyWindow, item.prefix + "-weekly"},
		} {
			w := spec.window
			if w.Window != spec.name || w.BucketID != spec.bucket {
				return fmt.Errorf("invalid quota window identity %q/%q", w.BucketID, w.Window)
			}
			if w.Status != "available" && w.Status != "disabled" && w.Status != "unavailable" {
				return fmt.Errorf("invalid quota window status %q", w.Status)
			}
			if w.Disabled != (w.Status == "disabled") {
				return errors.New("quota disabled flag disagrees with status")
			}
			for _, value := range []struct {
				pointer *float64
				max     float64
			}{
				{w.RemainingFraction, 1}, {w.RemainingPercent, 100}, {w.UsedPercent, 100},
			} {
				if value.pointer != nil && (math.IsNaN(*value.pointer) || math.IsInf(*value.pointer, 0) || *value.pointer < 0 || *value.pointer > value.max) {
					return errors.New("invalid quota percentage")
				}
			}
			if w.Status != "available" && (w.RemainingFraction != nil || w.RemainingPercent != nil || w.UsedPercent != nil) {
				return errors.New("unusable quota window contains percentages")
			}
		}
	}
	return nil
}

func cloneSnapshot(sample quota.Snapshot) quota.Snapshot {
	sample.Pools.Gemini.FiveHour = cloneWindow(sample.Pools.Gemini.FiveHour)
	sample.Pools.Gemini.Weekly = cloneWindow(sample.Pools.Gemini.Weekly)
	sample.Pools.ThirdParty.FiveHour = cloneWindow(sample.Pools.ThirdParty.FiveHour)
	sample.Pools.ThirdParty.Weekly = cloneWindow(sample.Pools.ThirdParty.Weekly)
	return sample
}

func cloneWindow(window quota.Window) quota.Window {
	window.RemainingFraction = cloneFloat(window.RemainingFraction)
	window.RemainingPercent = cloneFloat(window.RemainingPercent)
	window.UsedPercent = cloneFloat(window.UsedPercent)
	window.ResetAt = cloneTime(window.ResetAt)
	if window.RemainingAmount != nil {
		value := *window.RemainingAmount
		window.RemainingAmount = &value
	}
	return window
}

func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
