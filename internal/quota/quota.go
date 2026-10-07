// Package quota fetches and parses authoritative shared-pool quotas without estimating usage.
package quota

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

const (
	// GeminiPool identifies the shared Gemini model pool.
	GeminiPool = "gemini"
	// ThirdPartyPool identifies the shared Claude and GPT model pool.
	ThirdPartyPool = "third_party"
	// FiveHourWindow identifies the nominal five-hour quota window.
	FiveHourWindow = "5h"
	// WeeklyWindow identifies the nominal weekly quota window.
	WeeklyWindow = "weekly"
)

// Snapshot is one authoritative observation. Consumers must not mutate its pointers.
type Snapshot struct {
	ObservedAt time.Time `json:"observed_at"`
	Source     string    `json:"source"`
	Pools      Pools     `json:"pools"`
}

// Pools contains both known shared model pools, including unavailable windows.
type Pools struct {
	Gemini     Pool `json:"gemini"`
	ThirdParty Pool `json:"third_party"`
}

// Pool contains the two independently reported quota windows for one model pool.
type Pool struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	FiveHour    Window `json:"five_hour"`
	Weekly      Window `json:"weekly"`
}

// Window describes a known bucket. Nil values mean unreported or unusable data,
// never zero. RemainingAmount retains the upstream int64 string without units.
type Window struct {
	BucketID          string     `json:"bucket_id"`
	Window            string     `json:"window"`
	Status            string     `json:"status"`
	Disabled          bool       `json:"disabled"`
	RemainingFraction *float64   `json:"remaining_fraction"`
	RemainingPercent  *float64   `json:"remaining_percent"`
	UsedPercent       *float64   `json:"used_percent"`
	RemainingAmount   *string    `json:"remaining_amount"`
	ResetAt           *time.Time `json:"reset_at"`
	UnavailableReason string     `json:"unavailable_reason,omitempty"`
}

type rawBucket struct {
	BucketID          json.RawMessage `json:"bucketId"`
	DisplayName       json.RawMessage `json:"displayName"`
	Window            json.RawMessage `json:"window"`
	Disabled          json.RawMessage `json:"disabled"`
	RemainingFraction json.RawMessage `json:"remainingFraction"`
	RemainingAmount   json.RawMessage `json:"remainingAmount"`
	ResetTime         json.RawMessage `json:"resetTime"`
}

type reportedBucket struct {
	bucket    rawBucket
	groupName string
}

// Parse reads retrieveUserQuotaSummary groups or canonical buckets, optionally
// nested in response/summary wrappers. Only known bucket IDs determine pool and
// window; model names and reset distances never do. Invalid fields make that
// window unavailable; malformed envelopes and conflicting duplicates are errors.
// Every call owns its resulting pointer values independently of all other calls.
func Parse(body []byte, observedAt time.Time) (Snapshot, error) {
	snapshot := Snapshot{
		ObservedAt: observedAt,
		Source:     "retrieveUserQuotaSummary",
		Pools: Pools{
			Gemini:     Pool{ID: GeminiPool, FiveHour: missing("gemini-5h", FiveHourWindow), Weekly: missing("gemini-weekly", WeeklyWindow)},
			ThirdParty: Pool{ID: ThirdPartyPool, FiveHour: missing("3p-5h", FiveHourWindow), Weekly: missing("3p-weekly", WeeklyWindow)},
		},
	}
	var buckets []reportedBucket
	if err := collectBuckets(body, 0, &buckets); err != nil {
		return Snapshot{}, err
	}
	seen := make(map[string]rawBucket, 4)
	for _, reported := range buckets {
		var id string
		if json.Unmarshal(reported.bucket.BucketID, &id) != nil {
			continue
		}
		var target *Window
		var pool *Pool
		switch id {
		case "gemini-5h":
			pool, target = &snapshot.Pools.Gemini, &snapshot.Pools.Gemini.FiveHour
		case "gemini-weekly":
			pool, target = &snapshot.Pools.Gemini, &snapshot.Pools.Gemini.Weekly
		case "3p-5h":
			pool, target = &snapshot.Pools.ThirdParty, &snapshot.Pools.ThirdParty.FiveHour
		case "3p-weekly":
			pool, target = &snapshot.Pools.ThirdParty, &snapshot.Pools.ThirdParty.Weekly
		default:
			continue
		}
		if reported.groupName != "" {
			if pool.DisplayName != "" && pool.DisplayName != reported.groupName {
				return Snapshot{}, fmt.Errorf("conflicting quota pool display name for %s", pool.ID)
			}
			pool.DisplayName = reported.groupName
		}
		if previous, exists := seen[id]; exists {
			if bucketSignature(reported.bucket, target.Window) != bucketSignature(previous, target.Window) {
				return Snapshot{}, fmt.Errorf("conflicting duplicate quota bucket %s", id)
			}
			continue
		}
		seen[id] = reported.bucket
		*target = parseWindow(reported.bucket, *target)
	}
	return snapshot, nil
}

func missing(id, window string) Window {
	return Window{BucketID: id, Window: window, Status: "unavailable", UnavailableReason: "not_reported"}
}

func collectBuckets(body []byte, depth int, buckets *[]reportedBucket) error {
	if depth > 8 {
		return errors.New("quota response wrappers exceed nesting limit")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return errors.New("invalid quota response object")
	}
	if raw, exists := object["buckets"]; exists {
		var values []rawBucket
		if err := json.Unmarshal(raw, &values); err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("invalid quota buckets array")
		}
		for _, value := range values {
			*buckets = append(*buckets, reportedBucket{bucket: value})
		}
	}
	if raw, exists := object["groups"]; exists {
		var groups []struct {
			DisplayName string      `json:"displayName"`
			Buckets     []rawBucket `json:"buckets"`
		}
		if err := json.Unmarshal(raw, &groups); err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("invalid quota groups array")
		}
		for _, group := range groups {
			for _, value := range group.Buckets {
				*buckets = append(*buckets, reportedBucket{bucket: value, groupName: group.DisplayName})
			}
		}
	}
	for _, key := range []string{"response", "summary"} {
		if raw, exists := object[key]; exists {
			if err := collectBuckets(raw, depth+1, buckets); err != nil {
				return err
			}
		}
	}
	return nil
}

func present(raw json.RawMessage) bool {
	return len(raw) != 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func parseWindow(raw rawBucket, window Window) Window {
	reason := ""
	if present(raw.Window) {
		var value string
		if json.Unmarshal(raw.Window, &value) != nil || value != window.Window {
			reason = "invalid_window"
		}
	}
	if present(raw.Disabled) {
		if json.Unmarshal(raw.Disabled, &window.Disabled) != nil {
			reason = "invalid_disabled"
		}
	}
	if present(raw.ResetTime) {
		var value string
		if json.Unmarshal(raw.ResetTime, &value) != nil {
			reason = "invalid_reset_time"
		} else if reset, err := time.Parse(time.RFC3339Nano, value); err != nil {
			reason = "invalid_reset_time"
		} else {
			window.ResetAt = &reset
		}
	}
	if present(raw.RemainingAmount) {
		var value string
		if json.Unmarshal(raw.RemainingAmount, &value) != nil {
			reason = "invalid_remaining_amount"
		} else if _, err := strconv.ParseInt(value, 10, 64); err != nil {
			reason = "invalid_remaining_amount"
		} else {
			window.RemainingAmount = &value
		}
	}
	var fraction *float64
	if present(raw.RemainingFraction) {
		var value float64
		if json.Unmarshal(raw.RemainingFraction, &value) != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
			reason = "invalid_remaining_fraction"
		} else {
			fraction = &value
		}
	}
	if window.Disabled {
		window.Status, window.UnavailableReason = "disabled", "upstream_disabled"
		window.RemainingAmount = nil
		return window
	}
	if reason != "" {
		window.UnavailableReason = reason
		return window
	}
	if fraction == nil && window.RemainingAmount == nil {
		window.UnavailableReason = "quota_not_reported"
		return window
	}
	window.Status, window.UnavailableReason = "available", ""
	if fraction != nil {
		window.RemainingFraction = fraction
		remaining, used := *fraction*100, (1-*fraction)*100
		window.RemainingPercent, window.UsedPercent = &remaining, &used
	}
	return window
}

// Signatures compare source fields, including malformed values, so invalid
// duplicates cannot hide a conflict behind the same unavailable status.
func bucketSignature(raw rawBucket, nominalWindow string) string {
	if !present(raw.Window) {
		raw.Window, _ = json.Marshal(nominalWindow)
	}
	if !present(raw.Disabled) {
		raw.Disabled = json.RawMessage("false")
	}
	values := []json.RawMessage{raw.Window, raw.Disabled, raw.RemainingFraction, raw.RemainingAmount, raw.ResetTime, raw.DisplayName}
	canonical := make([]string, 0, len(values))
	for _, value := range values {
		if !present(value) {
			canonical = append(canonical, "null")
			continue
		}
		var decoded any
		if err := json.Unmarshal(value, &decoded); err != nil {
			canonical = append(canonical, string(bytes.TrimSpace(value)))
		} else {
			encoded, _ := json.Marshal(decoded)
			canonical = append(canonical, string(encoded))
		}
	}
	encoded, _ := json.Marshal(canonical)
	return string(encoded)
}
