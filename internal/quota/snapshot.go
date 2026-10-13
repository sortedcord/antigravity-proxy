package quota

import (
	"errors"
	"fmt"
	"math"
	"time"
)

const (
	StatusAvailable   WindowStatus = "available"
	StatusDisabled    WindowStatus = "disabled"
	StatusUnavailable WindowStatus = "unavailable"
)

// WindowStatus distinguishes authoritative values from disabled or unreported
// windows. Serialized strings remain stable; Validate rejects unknown values.
type WindowStatus string

// Bucket represents a single reported quota constraint bucket from upstream.
type Bucket struct {
	ID                string       `json:"id"`
	PoolDisplayName   string       `json:"pool_display_name,omitempty"`
	Window            string       `json:"window,omitempty"`
	DisplayName       string       `json:"display_name,omitempty"`
	Status            WindowStatus `json:"status"`
	RemainingFraction *float64     `json:"remaining_fraction,omitempty"`
	RemainingAmount   *string      `json:"remaining_amount,omitempty"`
	ResetAt           *time.Time   `json:"reset_at,omitempty"`
	UnavailableReason string       `json:"unavailable_reason,omitempty"`
}

// Clone returns an independent deep copy of the bucket.
func (b Bucket) Clone() Bucket {
	b.RemainingFraction = clonePointer(b.RemainingFraction)
	b.RemainingAmount = clonePointer(b.RemainingAmount)
	b.ResetAt = clonePointer(b.ResetAt)
	return b
}

// Validate checks individual bucket constraints.
func (b Bucket) Validate() error {
	if b.ID == "" {
		return errors.New("quota bucket requires a non-empty ID")
	}
	if b.Status != StatusAvailable && b.Status != StatusDisabled && b.Status != StatusUnavailable {
		return fmt.Errorf("invalid quota bucket status %q", b.Status)
	}
	if b.RemainingFraction != nil {
		f := *b.RemainingFraction
		if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1 {
			return errors.New("invalid quota remaining fraction")
		}
	}
	if b.Status != StatusAvailable && b.RemainingFraction != nil {
		return errors.New("unusable quota bucket contains remaining fraction")
	}
	return nil
}

// Snapshot is one authoritative observation. Consumers must not mutate its pointers.
type Snapshot struct {
	ObservedAt time.Time `json:"observed_at"`
	Source     string    `json:"source"`
	Buckets    []Bucket  `json:"buckets"`
}

// Bucket finds a bucket in the snapshot by ID.
func (s Snapshot) Bucket(id string) (Bucket, bool) {
	for _, b := range s.Buckets {
		if b.ID == id {
			return b, true
		}
	}
	return Bucket{}, false
}

// Clone gives consumers an independently mutable observation.
func (s Snapshot) Clone() Snapshot {
	c := s
	if s.Buckets != nil {
		c.Buckets = make([]Bucket, len(s.Buckets))
		for i, b := range s.Buckets {
			c.Buckets[i] = b.Clone()
		}
	} else {
		c.Buckets = []Bucket{}
	}
	return c
}

// Validate checks a complete observation before persistence, including loaded history.
func (s Snapshot) Validate() error {
	if s.ObservedAt.IsZero() || s.Source == "" {
		return errors.New("quota observation requires an observation time and source")
	}
	seen := make(map[string]bool, len(s.Buckets))
	for _, b := range s.Buckets {
		if seen[b.ID] {
			return fmt.Errorf("duplicate quota bucket %q in snapshot", b.ID)
		}
		seen[b.ID] = true
		if err := b.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
