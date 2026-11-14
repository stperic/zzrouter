package inventory

import (
	"math"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
)

// MemorySample retains measured usage independently of free/available memory.
// Peers send source-relative age; receivers replace ObservedAt with a local clock.
type MemorySample struct {
	TotalBytes int64     `json:"total_bytes"`
	UsedBytes  *int64    `json:"used_bytes,omitempty"`
	AgeSeconds float64   `json:"age_seconds"`
	ObservedAt time.Time `json:"observed_at"`
	Stale      bool      `json:"stale"`
}

// NewMemorySample copies a measurement, preserving an unknown used value.
func NewMemorySample(total int64, used *int64, observedAt time.Time) *MemorySample {
	if total <= 0 || observedAt.IsZero() {
		return nil
	}
	sample := &MemorySample{TotalBytes: total, ObservedAt: observedAt.UTC()}
	if used != nil && *used >= 0 {
		value := *used
		sample.UsedBytes = &value
	}
	return sample.Current(utils.Now(), false)
}

// Clone returns an independently mutable observation without renewing its age.
func (s *MemorySample) Clone() *MemorySample {
	if s == nil {
		return nil
	}
	copy := *s
	if s.UsedBytes != nil {
		value := *s.UsedBytes
		copy.UsedBytes = &value
	}
	return &copy
}

// Current returns a fresh projection without modifying the retained observation.
func (s *MemorySample) Current(now time.Time, unavailable bool) *MemorySample {
	copy := s.Clone()
	if copy != nil {
		copy.AgeSeconds = math.Max(0, now.Sub(s.ObservedAt).Seconds())
		copy.Stale = s.Stale || unavailable || s.ObservedAt.IsZero() || copy.AgeSeconds > (2*constants.HealthCheckInterval).Seconds()
	}
	return copy
}

// ReceiveMemorySample anchors peer age to local probe start, including transit time.
func ReceiveMemorySample(sample *MemorySample, startedAt time.Time) *MemorySample {
	if sample == nil || math.IsNaN(sample.AgeSeconds) || math.IsInf(sample.AgeSeconds, 0) || sample.AgeSeconds < 0 || sample.AgeSeconds >= float64(math.MaxInt64)/float64(time.Second) {
		return nil
	}
	result := NewMemorySample(sample.TotalBytes, sample.UsedBytes, startedAt.Add(-time.Duration(sample.AgeSeconds*float64(time.Second))))
	if result != nil {
		result.Stale = result.Stale || sample.Stale
	}
	return result
}
