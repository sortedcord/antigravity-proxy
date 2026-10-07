package status

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"antigravity-proxy/internal/quota"
)

// Query selects observation times inclusively and pages flattened quota windows.
// Zero-valued fields use the documented defaults. Fix To while paging if new
// observations must not shift the selected time range.
type Query struct {
	From   *time.Time
	To     *time.Time
	Pool   string
	Window string
	Limit  int
	Offset int
	Order  string
}

// Entry is one quota window at one observation, retaining unavailable and
// disabled states separately from an authoritative available zero.
type Entry struct {
	ObservedAt      time.Time `json:"observed_at"`
	Pool            string    `json:"pool"`
	PoolDisplayName string    `json:"pool_display_name"`
	quota.Window
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

// History returns independent entries ordered by observation time. Within each
// observation, ties are Gemini before third-party and five-hour before weekly,
// even for descending queries. Equal-time records use oldest append first for
// asc and newest append first for desc. Only the requested page is copied.
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
	if query.Pool != "all" && query.Pool != quota.GeminiPool && query.Pool != quota.ThirdPartyPool {
		return Result{}, fmt.Errorf("invalid quota pool %q", query.Pool)
	}
	if query.Window != "all" && query.Window != quota.FiveHourWindow && query.Window != quota.WeeklyWindow {
		return Result{}, fmt.Errorf("invalid quota window %q", query.Window)
	}
	if query.Limit < 1 || query.Limit > 1000 {
		return Result{}, errors.New("quota history limit must be between 1 and 1000")
	}
	if query.Offset < 0 {
		return Result{}, errors.New("quota history offset must be nonnegative")
	}
	if query.Order != "asc" && query.Order != "desc" {
		return Result{}, errors.New("quota history order must be asc or desc")
	}
	if query.From != nil && query.To != nil && query.From.After(*query.To) {
		return Result{}, errors.New("quota history from must not be after to")
	}
	result := Result{
		Entries: make([]Entry, 0), Limit: query.Limit, Offset: query.Offset,
		Order: query.Order, From: cloneTime(query.From), To: cloneTime(query.To),
		Pool: query.Pool, Window: query.Window,
	}
	// Canonical slots: Gemini five-hour, Gemini weekly, third-party five-hour,
	// third-party weekly. A fixed array avoids allocating all filtered entries.
	var slots [4]int
	selected := 0
	for slot := range 4 {
		if query.Pool == quota.GeminiPool && slot >= 2 || query.Pool == quota.ThirdPartyPool && slot < 2 {
			continue
		}
		if query.Window == quota.FiveHourWindow && slot%2 != 0 || query.Window == quota.WeeklyWindow && slot%2 != 1 {
			continue
		}
		slots[selected] = slot
		selected++
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
	result.Total = (last - first) * selected
	if query.Offset >= result.Total {
		return result, nil
	}
	count := result.Total - query.Offset
	if count > query.Limit {
		count = query.Limit
	}
	result.Entries = make([]Entry, 0, count)
	for flat := query.Offset; flat < query.Offset+count; flat++ {
		position := first + flat/selected
		if query.Order == "desc" {
			position = last - 1 - flat/selected
		}
		sample := &c.samples[c.ordered[position]]
		slot := slots[flat%selected]
		pool := &sample.Pools.Gemini
		if slot >= 2 {
			pool = &sample.Pools.ThirdParty
		}
		window := pool.FiveHour
		if slot%2 == 1 {
			window = pool.Weekly
		}
		result.Entries = append(result.Entries, Entry{
			ObservedAt: sample.ObservedAt, Pool: pool.ID, PoolDisplayName: pool.DisplayName, Window: cloneWindow(window),
		})
	}
	if count < result.Total-query.Offset {
		next := query.Offset + count
		result.NextOffset = &next
	}
	return result, nil
}
