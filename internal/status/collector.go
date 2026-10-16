// Package status collects authoritative quota observations in memory.
// Retention and disk persistence of historical metrics are disabled.
package status

import (
	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/quota"
	"context"
	"errors"
	"time"

	"github.com/terastal-hq/proxylib/poller"
)

// FetchFunc obtains one authoritative quota observation, respecting cancellation.
type FetchFunc = func(context.Context) (quota.Snapshot, error)

// PollInfo describes background polling attempts, successes, and errors.
type PollInfo = poller.PollInfo

// Collector owns in-memory quota observations and polling via poller.Poller.
type Collector struct {
	poller   *poller.Poller[quota.Snapshot]
	interval time.Duration
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
	p, err := poller.New(interval, func(ctx context.Context) (quota.Snapshot, error) {
		sample, err := fetch(ctx)
		if err != nil {
			return quota.Snapshot{}, err
		}
		if err := sample.Validate(); err != nil {
			return quota.Snapshot{}, err
		}
		return sample.Clone(), nil
	}, func(s quota.Snapshot) time.Time {
		return s.ObservedAt
	})
	if err != nil {
		return nil, err
	}
	return &Collector{poller: p, interval: interval}, nil
}

// Start begins background polling.
func (c *Collector) Start(ctx context.Context) error {
	if ctx == nil {
		return errors.New("quota polling context is nil")
	}
	return c.poller.Start(ctx)
}

func (c *Collector) poll(ctx context.Context) {
	c.poller.Poll(ctx)
}

// PollingInfo returns independent polling metadata without copying quota values.
func (c *Collector) PollingInfo() PollInfo {
	return c.poller.PollingInfo()
}

// Latest returns the most recent in-memory observation along with independent polling metadata.
func (c *Collector) Latest() (quota.Snapshot, bool, PollInfo) {
	snap, ok, info := c.poller.Latest()
	if !ok {
		return snap, false, info
	}
	return snap.Clone(), true, info
}

// TriggerPoll requests an immediate poll if the collector is started and not closed.
func (c *Collector) TriggerPoll() {
	c.poller.TriggerPoll()
}

// Done returns a channel that is closed when the background worker exits.
func (c *Collector) Done() <-chan struct{} {
	return c.poller.Done()
}

// Close stops background polling.
func (c *Collector) Close() error {
	return c.poller.Close()
}
