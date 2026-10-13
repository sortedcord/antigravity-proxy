package quota

import (
	"errors"
	"fmt"
	"math"
)

const (
	GeminiFiveHourBucket     = "gemini-5h"
	GeminiWeeklyBucket       = "gemini-weekly"
	ThirdPartyFiveHourBucket = "3p-5h"
	ThirdPartyWeeklyBucket   = "3p-weekly"

	StatusAvailable   WindowStatus = "available"
	StatusDisabled    WindowStatus = "disabled"
	StatusUnavailable WindowStatus = "unavailable"
)

// WindowStatus distinguishes authoritative values from disabled or unreported
// windows. Serialized strings remain stable; Validate rejects unknown values.
type WindowStatus string

// Bucket identifies a canonical slot and the snapshot-owned values it describes.
// Iteration order is Gemini before third-party and five-hour before weekly.
type Bucket struct {
	ID       string
	PoolID   string
	WindowID string
	Pool     *Pool
	Window   *Window
}

func (s *Snapshot) Buckets() [4]Bucket {
	return [4]Bucket{
		{ID: GeminiFiveHourBucket, PoolID: GeminiPool, WindowID: FiveHourWindow, Pool: &s.Pools.Gemini, Window: &s.Pools.Gemini.FiveHour},
		{ID: GeminiWeeklyBucket, PoolID: GeminiPool, WindowID: WeeklyWindow, Pool: &s.Pools.Gemini, Window: &s.Pools.Gemini.Weekly},
		{ID: ThirdPartyFiveHourBucket, PoolID: ThirdPartyPool, WindowID: FiveHourWindow, Pool: &s.Pools.ThirdParty, Window: &s.Pools.ThirdParty.FiveHour},
		{ID: ThirdPartyWeeklyBucket, PoolID: ThirdPartyPool, WindowID: WeeklyWindow, Pool: &s.Pools.ThirdParty, Window: &s.Pools.ThirdParty.Weekly},
	}
}

// Validate checks a complete observation before persistence, including loaded
// history. It does not manufacture percentages or require unreported values.
func (s Snapshot) Validate() error {
	if s.ObservedAt.IsZero() || s.Source == "" {
		return errors.New("quota observation requires an observation time and source")
	}
	for _, bucket := range s.Buckets() {
		if bucket.Pool.ID != bucket.PoolID {
			return fmt.Errorf("invalid quota pool %q", bucket.Pool.ID)
		}
		w := bucket.Window
		if w.Window != bucket.WindowID || w.BucketID != bucket.ID {
			return fmt.Errorf("invalid quota window identity %q/%q", w.BucketID, w.Window)
		}
		if w.Status != StatusAvailable && w.Status != StatusDisabled && w.Status != StatusUnavailable {
			return fmt.Errorf("invalid quota window status %q", w.Status)
		}
		if w.Disabled != (w.Status == StatusDisabled) {
			return errors.New("quota disabled flag disagrees with status")
		}
		for _, value := range []struct {
			pointer *float64
			max     float64
		}{
			{pointer: w.RemainingFraction, max: 1}, {pointer: w.RemainingPercent, max: 100}, {pointer: w.UsedPercent, max: 100},
		} {
			if value.pointer != nil && (math.IsNaN(*value.pointer) || math.IsInf(*value.pointer, 0) || *value.pointer < 0 || *value.pointer > value.max) {
				return errors.New("invalid quota percentage")
			}
		}
		if w.Status != StatusAvailable && (w.RemainingFraction != nil || w.RemainingPercent != nil || w.UsedPercent != nil) {
			return errors.New("unusable quota window contains percentages")
		}
	}
	return nil
}

// Clone gives consumers an independently mutable observation, including all
// optional fields. Pointer values in the original remain snapshot-owned.
func (s Snapshot) Clone() Snapshot {
	for _, bucket := range s.Buckets() {
		*bucket.Window = bucket.Window.Clone()
	}
	return s
}

func (w Window) Clone() Window {
	w.RemainingFraction = clonePointer(w.RemainingFraction)
	w.RemainingPercent = clonePointer(w.RemainingPercent)
	w.UsedPercent = clonePointer(w.UsedPercent)
	w.RemainingAmount = clonePointer(w.RemainingAmount)
	w.ResetAt = clonePointer(w.ResetAt)
	return w
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
