package status

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"antigravity-proxy/internal/config"
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
}

// NewService configures status without starting work or opening history files.
func NewService(cfg config.Config, fetch FetchFunc) *Service {
	return &Service{historyPath: cfg.QuotaHistoryPath, intervalSeconds: cfg.QuotaPollIntervalSeconds, maxSamples: cfg.QuotaHistoryMaxSamples, fetch: fetch}
}

// Start opens persistent quota history and starts account-scoped polling.
// It is explicit so constructing a Service does not start background work or write files.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.collector != nil {
		return errors.New("quota status polling is already started")
	}
	seconds := s.intervalSeconds
	if seconds == 0 {
		seconds = config.DefaultQuotaPollIntervalSeconds
	}
	if seconds < 1 || int64(seconds) > int64(1<<63-1)/int64(time.Second) {
		return errors.New("quota polling interval is outside the supported duration range")
	}
	collector, err := Open(s.historyPath, time.Duration(seconds)*time.Second, s.maxSamples, s.fetch)
	if err != nil {
		return fmt.Errorf("open quota history: %w", err)
	}
	if err := collector.Start(ctx); err != nil {
		_ = collector.Close()
		return fmt.Errorf("start quota polling: %w", err)
	}
	s.collector = collector
	return nil
}

// Close cancels polling, waits for the active fetch to finish, and closes
// the history file. It is safe to call when polling has not been started.
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.collector == nil {
		return nil
	}
	err := s.collector.Close()
	s.collector = nil
	return err
}

func (s *Service) quotaCollector() *Collector {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.collector
}

// ServeHTTP serves limit and usage after the outer router authenticates requests.
// It never fetches quota synchronously; only the polling worker contacts upstream.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "status endpoints require GET"})
		return
	}
	collector := s.quotaCollector()
	if collector == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "quota polling is not started"})
		return
	}
	if r.URL.Path == "/status/limit" {
		snapshot, available, polling := collector.Latest()
		if !available {
			message := "quota data has not been observed yet"
			if polling.LastError != "" {
				message = polling.LastError
			}
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": message, "polling": polling})
			return
		}
		stale := snapshotStale(snapshot.ObservedAt, collector.interval, polling.LastError, time.Now())
		writeJSON(w, http.StatusOK, struct {
			quota.Snapshot
			Polling PollInfo `json:"polling"`
			Stale   bool     `json:"stale"`
		}{Snapshot: snapshot, Polling: polling, Stale: stale})
		return
	}
	query, err := parseUsageQuery(r.URL.RawQuery)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	result, err := collector.History(query)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	polling := collector.PollingInfo()
	writeJSON(w, http.StatusOK, struct {
		Result
		Polling PollInfo `json:"polling"`
	}{Result: result, Polling: polling})
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
	if query.Pool != "all" && query.Pool != "gemini" && query.Pool != "third_party" {
		return query, errors.New("pool must be all, gemini, or third_party")
	}
	if query.Window != "all" && query.Window != "5h" && query.Window != "weekly" {
		return query, errors.New("window must be all, 5h, or weekly")
	}
	if query.Limit < 1 || query.Limit > 1000 {
		return query, errors.New("limit must be between 1 and 1000")
	}
	if query.Offset < 0 {
		return query, errors.New("offset must be nonnegative")
	}
	if query.Order != "asc" && query.Order != "desc" {
		return query, errors.New("order must be asc or desc")
	}
	if query.From != nil && query.To != nil && query.From.After(*query.To) {
		return query, errors.New("from must not be after to")
	}
	return query, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
