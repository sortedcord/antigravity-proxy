package status

import (
	"antigravity-proxy/internal/quota"
	"errors"
	"fmt"
	"time"

	"github.com/terastal-hq/proxylib/poller"
)

// Query selects observation times inclusively and pages flattened quota buckets.
type Query struct {
	From   *time.Time
	To     *time.Time
	Pool   string
	Window string
	Limit  int
	Offset int
	Order  string
}

// Entry is one quota bucket at one observation.
type Entry struct {
	ObservedAt time.Time `json:"observed_at"`
	quota.Bucket
}

// Result contains a bounded page and a total calculated before pagination.
type Result struct {
	Entries    []Entry    `json:"entries"`
	Total      int        `json:"total"`
	Limit      int        `json:"limit"`
	Offset     int        `json:"offset"`
	NextOffset *int       `json:"next_offset"`
	Order      string     `json:"order"`
	From       *time.Time `json:"from,omitempty"`
	To         *time.Time `json:"to,omitempty"`
	Pool       string     `json:"pool"`
	Window     string     `json:"window"`
}

// History returns an empty result page because historical metrics persistence is disabled.
func (c *Collector) History(query Query) (Result, error) {
	if query.Pool == "" {
		query.Pool = "all"
	}
	if query.Window == "" {
		query.Window = "all"
	}
	if query.Limit == 0 {
		query.Limit = 100
	}
	if query.Order == "" {
		query.Order = "desc"
	}
	if err := query.Validate(); err != nil {
		return Result{}, err
	}
	result := Result{
		Entries: make([]Entry, 0), Limit: query.Limit, Offset: query.Offset,
		Order: query.Order, From: poller.CloneTime(query.From), To: poller.CloneTime(query.To),
		Pool: query.Pool, Window: query.Window,
	}
	return result, nil
}

// Validate checks filters without applying defaults. This keeps explicit HTTP
// limit=0 invalid while allowing zero-value programmatic queries to default.
func (query Query) Validate() error {
	if query.Pool != "all" && query.Pool != "gemini" && query.Pool != "third_party" && query.Pool != "" {
		return fmt.Errorf("invalid quota pool %q", query.Pool)
	}
	if query.Window != "all" && query.Window != "5h" && query.Window != "weekly" && query.Window != "" {
		return fmt.Errorf("invalid quota window %q", query.Window)
	}
	if query.Limit < 1 || query.Limit > 1000 {
		return errors.New("quota history limit must be between 1 and 1000")
	}
	if query.Offset < 0 {
		return errors.New("quota history offset must be nonnegative")
	}
	if query.Order != "asc" && query.Order != "desc" {
		return errors.New("quota history order must be asc or desc")
	}
	if query.From != nil && query.To != nil && query.From.After(*query.To) {
		return errors.New("quota history from must not be after to")
	}
	return nil
}
