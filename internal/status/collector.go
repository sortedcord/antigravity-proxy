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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/lifecycle"
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

	trigger chan struct{}

	started    bool
	closed     bool
	cancel     context.CancelFunc
	workerDone chan struct{}
	closer     lifecycle.Closer
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
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
		interval: interval, fetch: fetch, trigger: make(chan struct{}, 1),
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
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fail(fmt.Errorf("open quota history: %w", err))
	}
	c.file = file
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return fail(errors.New("quota history must be a regular file"))
	}
	if err := file.Chmod(0o600); err != nil {
		return fail(fmt.Errorf("protect quota history: %w", err))
	}
	if err := c.loadHistory(file); err != nil {
		return fail(err)
	}
	c.rebuildIndexLocked()
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
		case <-c.trigger:
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
		err = sample.Validate()
	}
	if err == nil {
		sample = sample.Clone()
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
	if len(c.samples) == c.maxSamples {
		copy(c.samples, c.samples[1:])
		c.samples[len(c.samples)-1] = sample
	} else {
		c.samples = append(c.samples, sample)
	}
	c.rebuildIndexLocked()
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
	return c.samples[len(c.samples)-1].Clone(), true, info
}

// TriggerPoll requests an immediate poll if the collector is started and not closed.
func (c *Collector) TriggerPoll() {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed || !c.started {
		return
	}
	select {
	case c.trigger <- struct{}{}:
	default:
	}
}

// Close cancels and joins the polling worker before closing storage. It is
// idempotent, safe concurrently, and valid even when Start was never called.
func (c *Collector) Close() error {
	return c.closer.Close(func() error {
		c.mu.Lock()
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
		defer c.mu.Unlock()
		var err error
		if c.file != nil {
			err = c.file.Close()
		}
		return errors.Join(err, c.lock.Close())
	})
}

func (c *Collector) rebuildIndexLocked() {
	if cap(c.ordered) < len(c.samples) {
		c.ordered = make([]int, len(c.samples))
	} else {
		c.ordered = c.ordered[:len(c.samples)]
	}
	for i := range c.ordered {
		c.ordered[i] = i
	}
	sort.SliceStable(c.ordered, func(i, j int) bool {
		return c.samples[c.ordered[i]].ObservedAt.Before(c.samples[c.ordered[j]].ObservedAt)
	})
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
