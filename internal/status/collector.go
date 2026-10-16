// Package status collects authoritative quota observations in memory.
// Retention and disk persistence of historical metrics are disabled.
package status

import (
	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/lifecycle"
	"antigravity-proxy/internal/quota"
	"context"
	"errors"
	"sync"
	"time"
)

// FetchFunc obtains one authoritative quota observation, respecting cancellation.
type FetchFunc = func(context.Context) (quota.Snapshot, error)

// PollInfo describes attempted and successful collection.
type PollInfo struct {
	IntervalSeconds int64      `json:"interval_seconds"`
	LastAttemptAt   *time.Time `json:"last_attempt_at"`
	LastSuccessAt   *time.Time `json:"last_success_at"`
	LastError       string     `json:"last_error,omitempty"`
}

// Collector owns in-memory quota observations and at most one nonoverlapping polling worker.
// Its public methods are safe for concurrent use.
type Collector struct {
	mu        sync.RWMutex
	interval  time.Duration
	fetch     FetchFunc
	latest    quota.Snapshot
	hasLatest bool
	polling   PollInfo

	trigger chan struct{}

	started    bool
	closed     bool
	cancel     context.CancelFunc
	workerDone chan struct{}
	closer     lifecycle.Closer
}

// Open initializes in-memory quota tracking without touching disk.
func Open(path string, interval time.Duration, maxSamples int, fetch FetchFunc) (*Collector, error) {
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
	c := &Collector{
		interval: interval,
		fetch:    fetch,
		trigger:  make(chan struct{}, 1),
		polling:  PollInfo{IntervalSeconds: int64(interval / time.Second)},
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
	if err != nil {
		c.polling.LastError = err.Error()
		return
	}
	c.latest = sample
	c.hasLatest = true
	at := sample.ObservedAt
	c.polling.LastSuccessAt = &at
	c.polling.LastError = ""
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

// Latest returns the most recent in-memory observation along with independent polling metadata.
func (c *Collector) Latest() (quota.Snapshot, bool, PollInfo) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	info := c.pollingInfoLocked()
	if !c.hasLatest {
		return quota.Snapshot{}, false, info
	}
	return c.latest.Clone(), true, info
}

// TriggerPoll requests an immediate poll if the collector is started and not closed.
func (c *Collector) TriggerPoll() {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.started || c.closed {
		return
	}
	select {
	case c.trigger <- struct{}{}:
	default:
	}
}

// Close stops background polling.
func (c *Collector) Close() error {
	return c.closer.Close(func() error {
		c.mu.Lock()
		c.closed = true
		if c.cancel != nil {
			c.cancel()
		}
		c.mu.Unlock()

		if c.workerDone != nil {
			<-c.workerDone
		}
		return nil
	})
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	cloned := *t
	return &cloned
}
