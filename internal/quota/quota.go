// Package quota fetches and parses authoritative shared-pool quotas without estimating usage.
package quota

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

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
// nested in response/summary wrappers. Only reported bucket IDs produce entries.
// Malformed envelopes and conflicting duplicates are errors.
// Every call owns its resulting pointer values independently of all other calls.
func Parse(body []byte, observedAt time.Time) (Snapshot, error) {
	snapshot := Snapshot{
		ObservedAt: observedAt,
		Source:     "retrieveUserQuotaSummary",
		Buckets:    make([]Bucket, 0),
	}

	var rawBuckets []reportedBucket
	if err := collectBuckets(body, 0, &rawBuckets); err != nil {
		return Snapshot{}, err
	}

	seenSignatures := make(map[string]string)
	parsedBuckets := make(map[string]Bucket)
	groupDisplayNames := make(map[string]string)

	for _, reported := range rawBuckets {
		b, err := parseBucket(reported.bucket, reported.groupName)
		if err != nil {
			return Snapshot{}, err
		}

		groupKey := ""
		if strings.HasPrefix(b.ID, "gemini-") {
			groupKey = "gemini"
		} else if strings.HasPrefix(b.ID, "3p-") {
			groupKey = "3p"
		}
		if groupKey != "" && reported.groupName != "" {
			if existing, ok := groupDisplayNames[groupKey]; ok && existing != reported.groupName {
				return Snapshot{}, fmt.Errorf("conflicting quota pool display name for %s", groupKey)
			}
			groupDisplayNames[groupKey] = reported.groupName
		}

		sig := bucketSignature(reported.bucket)
		if prevSig, exists := seenSignatures[b.ID]; exists {
			previous := parsedBuckets[b.ID]
			if prevSig != sig || (previous.PoolDisplayName != "" && b.PoolDisplayName != "" && previous.PoolDisplayName != b.PoolDisplayName) {
				return Snapshot{}, fmt.Errorf("conflicting duplicate quota bucket %s", b.ID)
			}
			// A repeated bucket may add group metadata through a groups wrapper.
			if previous.PoolDisplayName == "" {
				previous.PoolDisplayName = b.PoolDisplayName
				parsedBuckets[b.ID] = previous
			}
			continue
		}
		seenSignatures[b.ID] = sig
		parsedBuckets[b.ID] = b
	}

	buckets := make([]Bucket, 0, len(parsedBuckets))
	for _, b := range parsedBuckets {
		buckets = append(buckets, b)
	}
	slices.SortFunc(buckets, func(a, b Bucket) int {
		return strings.Compare(a.ID, b.ID)
	})

	snapshot.Buckets = buckets
	return snapshot, nil
}

func bucketSignature(raw rawBucket) string {
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

func parseBucket(raw rawBucket, groupName string) (Bucket, error) {
	var id string
	if err := json.Unmarshal(raw.BucketID, &id); err != nil || strings.TrimSpace(id) == "" {
		return Bucket{}, errors.New("invalid or missing bucketId")
	}

	var displayName string
	if present(raw.DisplayName) {
		_ = json.Unmarshal(raw.DisplayName, &displayName)
	}

	reason := ""
	var window string
	if present(raw.Window) {
		if json.Unmarshal(raw.Window, &window) != nil {
			reason = "invalid_window"
		}
	}
	var disabled bool
	if present(raw.Disabled) {
		if json.Unmarshal(raw.Disabled, &disabled) != nil {
			reason = "invalid_disabled"
		}
	}

	var resetAt *time.Time
	if present(raw.ResetTime) {
		var val string
		if json.Unmarshal(raw.ResetTime, &val) != nil {
			reason = "invalid_reset_time"
		} else if parsed, err := time.Parse(time.RFC3339Nano, val); err != nil {
			reason = "invalid_reset_time"
		} else {
			resetAt = &parsed
		}
	}

	var remainingAmount *string
	if present(raw.RemainingAmount) {
		var val string
		if json.Unmarshal(raw.RemainingAmount, &val) != nil {
			reason = "invalid_remaining_amount"
		} else if _, err := strconv.ParseInt(val, 10, 64); err != nil {
			reason = "invalid_remaining_amount"
		} else {
			remainingAmount = &val
		}
	}

	var remainingFraction *float64
	if present(raw.RemainingFraction) {
		var val float64
		if json.Unmarshal(raw.RemainingFraction, &val) != nil || math.IsNaN(val) || math.IsInf(val, 0) || val < 0 || val > 1 {
			reason = "invalid_remaining_fraction"
		} else {
			remainingFraction = &val
		}
	}

	status := StatusAvailable
	unavailableReason := ""
	switch {
	case disabled:
		status = StatusDisabled
		unavailableReason = "upstream_disabled"
		remainingAmount = nil
		remainingFraction = nil
	case reason != "":
		status = StatusUnavailable
		unavailableReason = reason
		remainingAmount = nil
		remainingFraction = nil
	case remainingFraction == nil && remainingAmount == nil:
		status = StatusUnavailable
		unavailableReason = "quota_not_reported"
	}

	return Bucket{
		ID:                id,
		PoolDisplayName:   groupName,
		Window:            window,
		DisplayName:       displayName,
		Status:            status,
		RemainingFraction: remainingFraction,
		RemainingAmount:   remainingAmount,
		ResetAt:           resetAt,
		UnavailableReason: unavailableReason,
	}, nil
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
