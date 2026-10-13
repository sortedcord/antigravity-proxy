package status

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"antigravity-proxy/internal/httpjson"

	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/lifecycle"
	"antigravity-proxy/internal/quota"
)

// Service owns account-scoped polling and its HTTP status API. Construct it,
// wire it into the authenticated router, and explicitly Start and Close it.
type Service struct {
	historyPath     string
	intervalSeconds int
	maxSamples      int
	fetch           FetchFunc
	mu              sync.RWMutex
	collector       *Collector
	closed          bool
	closer          lifecycle.Closer
}

// Options contains only the settings owned by account-scoped status collection.
type Options struct {
	HistoryPath         string
	PollIntervalSeconds int
	MaxSamples          int
}

// New configures status without opening files or starting work.
func New(options Options, fetch FetchFunc) *Service {
	return &Service{historyPath: options.HistoryPath, intervalSeconds: options.PollIntervalSeconds, maxSamples: options.MaxSamples, fetch: fetch}
}

// NewService is the configuration-based compatibility constructor. New callers
// may use New with Options to avoid depending on unrelated application settings.
func NewService(cfg config.Config, fetch FetchFunc) *Service {
	return New(Options{HistoryPath: cfg.QuotaHistoryPath, PollIntervalSeconds: cfg.QuotaPollIntervalSeconds, MaxSamples: cfg.QuotaHistoryMaxSamples}, fetch)
}

// Prepare opens and validates persistent history and holds its exclusive lock.
// It does not start a worker or fetch quota. Repeated preparation is harmless.
func (s *Service) Prepare() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prepareLocked()
}

func (s *Service) prepareLocked() error {
	if s.closed {
		return errors.New("quota status service is closed")
	}
	if s.collector != nil {
		return nil
	}
	seconds := s.intervalSeconds
	if seconds == 0 {
		seconds = config.DefaultQuotaPollIntervalSeconds
	}
	if err := config.ValidatePollInterval(seconds); err != nil {
		return errors.New("quota polling interval is outside the supported duration range")
	}
	collector, err := Open(s.historyPath, time.Duration(seconds)*time.Second, s.maxSamples, s.fetch)
	if err != nil {
		return fmt.Errorf("open quota history: %w", err)
	}
	s.collector = collector
	return nil
}

// Start begins polling prepared history, preparing it first if necessary.
// A Service can start only once, including when its parent context is canceled.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("quota status service is closed")
	}
	if ctx == nil {
		return errors.New("quota polling context is nil")
	}
	if err := s.prepareLocked(); err != nil {
		return err
	}
	if err := s.collector.Start(ctx); err != nil {
		return fmt.Errorf("start quota polling: %w", err)
	}
	return nil
}

// Close cancels and joins polling and releases even prepared, unstarted history.
// It is terminal, idempotent, and safe concurrently with reads and lifecycle calls.
func (s *Service) Close() error {
	return s.closer.Close(func() error {
		s.mu.Lock()
		s.closed = true
		collector := s.collector
		s.mu.Unlock()
		var err error
		if collector != nil {
			err = collector.Close()
		}
		s.mu.Lock()
		s.collector = nil
		s.mu.Unlock()
		return err
	})
}

func (s *Service) quotaCollector() *Collector {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.collector
}

// QuotaCollector returns the active quota collector if running.
func (s *Service) QuotaCollector() *Collector {
	return s.quotaCollector()
}

// TriggerPoll requests an immediate poll if quota collection is active.
func (s *Service) TriggerPoll() {
	if c := s.quotaCollector(); c != nil {
		c.TriggerPoll()
	}
}

// AccountHistoryPath isolates every namespace in a stable, filesystem-safe path.
// The unsuffixed legacy history is never selected, even for an empty namespace.
func AccountHistoryPath(basePath, accountID string) string {
	dir := filepath.Dir(basePath)
	ext := filepath.Ext(basePath)
	base := strings.TrimSuffix(filepath.Base(basePath), ext)
	digest := sha256.Sum256([]byte(accountID))
	return filepath.Join(dir, fmt.Sprintf("%s-%x%s", base, digest, ext))
}

// ServeHTTP serves limit and usage after the outer router authenticates requests.
// It never fetches quota synchronously; only the polling worker contacts upstream.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		httpjson.Write(w, http.StatusMethodNotAllowed, map[string]string{"error": "status endpoints require GET"})
		return
	}
	collector := s.quotaCollector()
	if collector == nil {
		httpjson.Write(w, http.StatusServiceUnavailable, map[string]string{"error": "quota polling is not started"})
		return
	}
	if r.URL.Path == "/status/limit" {
		snapshot, available, polling := collector.Latest()
		if !available {
			message := "quota data has not been observed yet"
			if polling.LastError != "" {
				message = polling.LastError
			}
			httpjson.Write(w, http.StatusServiceUnavailable, map[string]any{"error": message, "polling": polling})
			return
		}
		stale := snapshotStale(snapshot.ObservedAt, collector.interval, polling.LastError, time.Now())
		httpjson.Write(w, http.StatusOK, struct {
			quota.Snapshot
			Polling PollInfo `json:"polling"`
			Stale   bool     `json:"stale"`
		}{Snapshot: snapshot, Polling: polling, Stale: stale})
		return
	}
	query, err := parseUsageQuery(r.URL.RawQuery)
	if err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	result, err := collector.History(query)
	if err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	polling := collector.PollingInfo()
	httpjson.Write(w, http.StatusOK, struct {
		Result
		Polling PollInfo `json:"polling"`
	}{Result: result, Polling: polling})
}

// SnapshotStale determines whether an observation is stale based on the poll interval and errors.
func SnapshotStale(observedAt time.Time, interval time.Duration, lastError string, now time.Time) bool {
	return snapshotStale(observedAt, interval, lastError, now)
}

// snapshotStale allows two full polling intervals for an observation to land.
// Adding the intervals separately avoids overflowing a doubled Duration.
func snapshotStale(observedAt time.Time, interval time.Duration, lastError string, now time.Time) bool {
	return lastError != "" || now.After(observedAt.Add(interval).Add(interval))
}

func parseUsageQuery(raw string) (Query, error) {
	query := Query{Pool: "all", Window: "all", Limit: 100, Order: "desc"}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return query, errors.New("invalid usage query")
	}
	for name, entries := range values {
		if name == "key" {
			continue // Authentication query parameter is not a history filter.
		}
		if len(entries) != 1 || entries[0] == "" {
			return query, fmt.Errorf("%s must have exactly one nonempty value", name)
		}
		value := entries[0]
		switch name {
		case "from", "to":
			parsed, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return query, fmt.Errorf("%s must be an RFC3339 timestamp", name)
			}
			parsed = parsed.UTC()
			if name == "from" {
				query.From = &parsed
			} else {
				query.To = &parsed
			}
		case "pool":
			query.Pool = value
		case "window":
			query.Window = value
		case "limit", "offset":
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return query, fmt.Errorf("%s must be an integer", name)
			}
			if name == "limit" {
				query.Limit = parsed
			} else {
				query.Offset = parsed
			}
		case "order":
			query.Order = value
		default:
			return query, fmt.Errorf("unknown usage parameter %q", name)
		}
	}
	return query, query.Validate()
}
