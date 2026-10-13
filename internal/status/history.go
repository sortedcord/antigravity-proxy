package status

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"antigravity-proxy/internal/quota"
)

// Query selects observation times inclusively and pages flattened quota buckets.
// Zero-valued fields use the documented defaults. Fix To while paging if new
// observations must not shift the selected time range. Count-based retention
// may still evict older appends, even when their observation times are newer.
type Query struct {
	From   *time.Time
	To     *time.Time
	Pool   string
	Window string
	Limit  int
	Offset int
	Order  string
}

// Entry is one quota bucket at one observation, retaining unavailable and
// disabled states separately from an authoritative available zero.
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

// History returns independent entries ordered by observation time.
// Equal-time records use oldest append first for asc and newest append first for desc.
// Only the requested page is copied.
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
		Order: query.Order, From: cloneTime(query.From), To: cloneTime(query.To),
		Pool: query.Pool, Window: query.Window,
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	first, last := 0, len(c.ordered)
	if result.From != nil {
		first = sort.Search(len(c.ordered), func(i int) bool {
			return !c.samples[c.ordered[i]].ObservedAt.Before(*result.From)
		})
	}
	if result.To != nil {
		last = sort.Search(len(c.ordered), func(i int) bool {
			return c.samples[c.ordered[i]].ObservedAt.After(*result.To)
		})
	}

	matchBucket := func(b quota.Bucket) bool {
		if query.Pool != "all" {
			p := strings.ToLower(query.Pool)
			if p == "gemini" && !strings.HasPrefix(b.ID, "gemini-") {
				return false
			}
			if (p == "third_party" || p == "3p") && !strings.HasPrefix(b.ID, "3p-") {
				return false
			}
			if p != "gemini" && p != "third_party" && p != "3p" && !strings.EqualFold(b.PoolDisplayName, query.Pool) {
				return false
			}
		}
		if query.Window != "all" && b.Window != query.Window {
			return false
		}
		return true
	}

	total := 0
	for i := first; i < last; i++ {
		sample := &c.samples[c.ordered[i]]
		for _, b := range sample.Buckets {
			if matchBucket(b) {
				total++
			}
		}
	}
	result.Total = total
	if query.Offset >= result.Total {
		return result, nil
	}

	count := result.Total - query.Offset
	if count > query.Limit {
		count = query.Limit
	}
	result.Entries = make([]Entry, 0, count)

	current := 0
	collectSample := func(sample *quota.Snapshot) {
		for _, b := range sample.Buckets {
			if matchBucket(b) {
				if current >= query.Offset && len(result.Entries) < count {
					result.Entries = append(result.Entries, Entry{
						ObservedAt: sample.ObservedAt,
						Bucket:     b.Clone(),
					})
				}
				current++
			}
		}
	}

	if query.Order == "desc" {
		for i := last - 1; i >= first; i-- {
			collectSample(&c.samples[c.ordered[i]])
			if len(result.Entries) == count {
				break
			}
		}
	} else {
		for i := first; i < last; i++ {
			collectSample(&c.samples[c.ordered[i]])
			if len(result.Entries) == count {
				break
			}
		}
	}

	if count < result.Total-query.Offset {
		next := query.Offset + count
		result.NextOffset = &next
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
