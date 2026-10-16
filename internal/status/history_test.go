package status

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"antigravity-proxy/internal/quota"
)

func TestHistoryInvalidQueries(t *testing.T) {
	c := openTestCollector(t, filepath.Join(t.TempDir(), "history"), time.Second, func(context.Context) (quota.Snapshot, error) {
		return quota.Snapshot{}, nil
	})
	from, to := observationTime().Add(time.Second), observationTime()
	for _, query := range []Query{{Limit: -1}, {Limit: 1001}, {Offset: -1}, {Order: "newest"}, {From: &from, To: &to}} {
		if _, err := c.History(query); err == nil {
			t.Errorf("accepted invalid query %+v", query)
		}
	}
}
