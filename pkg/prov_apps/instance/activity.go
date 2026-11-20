package instance

import (
	"math"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
)

// Activity is observed work, independent of the protective engine-busy hold.
type Activity struct {
	State           string    `json:"state"`
	ActiveRequests  int64     `json:"active_requests"`
	BusyEngines     int       `json:"busy_engines"`
	ObservedAt      time.Time `json:"observed_at"`
	AgeSeconds      float64   `json:"age_seconds"`
	ValidForSeconds float64   `json:"valid_for_seconds"`
	Stale           bool      `json:"stale"`
}

type activityObservation struct {
	known    bool
	at       time.Time
	validFor time.Duration
}

func (o activityObservation) unknown(now time.Time) bool {
	return o.validFor > 0 && (!o.known || !now.Before(o.at.Add(o.validFor)))
}

// ConfigureActivityObservation declares expected evidence before the first probe.
func (i *Instance) ConfigureActivityObservation(validFor time.Duration) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.activityObservation = activityObservation{validFor: validFor}
}

// ActivityUnknown invalidates probe evidence and retains the positive hold until requested otherwise.
func (i *Instance) ActivityUnknown(releaseHold bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.activityObservation.known = false
	if releaseHold {
		i.observeActivityHoldLocked(false)
	}
}

// ActivitySnapshot reads admitted requests and fresh engine observations under one lock.
func (i *Instance) ActivitySnapshot(now time.Time) Activity {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.activitySnapshotLocked(now)
}

func (i *Instance) activitySnapshotLocked(now time.Time) Activity {
	result := Activity{State: "idle", ObservedAt: now.UTC(), ValidForSeconds: (2 * constants.HealthCheckInterval).Seconds()}
	if i.Status.IsTerminal() {
		return result
	}
	result.ActiveRequests = i.GetActiveRequests()
	probe := i.activityObservation
	if probe.validFor > 0 {
		if probe.unknown(now) {
			result.State, result.ValidForSeconds = "unknown", 0
		} else {
			remaining := probe.at.Add(probe.validFor).Sub(now).Seconds()
			result.ValidForSeconds = math.Min(result.ValidForSeconds, remaining)
			result.ObservedAt = probe.at.UTC()
			result.AgeSeconds = math.Max(0, now.Sub(probe.at).Seconds())
			if i.activityHold {
				result.BusyEngines = 1
			}
		}
	}
	if result.ActiveRequests > 0 {
		result.State = "active"
		result.ObservedAt, result.AgeSeconds = now.UTC(), 0
		result.ValidForSeconds = (2 * constants.HealthCheckInterval).Seconds()
	} else if result.BusyEngines > 0 {
		result.State = "active"
	}
	return result
}

// Current ages an immutable snapshot using the receiving node's clock.
func (a *Activity) Current(now time.Time, unavailable bool) *Activity {
	if a == nil {
		return nil
	}
	result := *a
	result.AgeSeconds = math.Max(0, now.Sub(a.ObservedAt).Seconds())
	result.ValidForSeconds = math.Max(0, a.ValidForSeconds-math.Max(0, result.AgeSeconds-a.AgeSeconds))
	result.Stale = a.Stale || unavailable || a.ObservedAt.IsZero() || result.ValidForSeconds <= 0
	if result.Stale {
		result.State = "unknown"
		result.ValidForSeconds = 0
	}
	return &result
}

// ReceiveActivity anchors a peer's remaining validity to local probe start.
func ReceiveActivity(a *Activity, startedAt time.Time) *Activity {
	if a == nil || (a.State != "active" && a.State != "idle" && a.State != "unknown") ||
		math.IsNaN(a.AgeSeconds) || math.IsInf(a.AgeSeconds, 0) || a.AgeSeconds < 0 || a.AgeSeconds >= float64(math.MaxInt64)/float64(time.Second) ||
		math.IsNaN(a.ValidForSeconds) || math.IsInf(a.ValidForSeconds, 0) || a.ValidForSeconds < 0 || a.ActiveRequests < 0 || a.BusyEngines < 0 {
		return nil
	}
	if (a.State == "active") != (a.ActiveRequests > 0 || a.BusyEngines > 0) && !a.Stale {
		return nil
	}
	result := *a
	result.ObservedAt = startedAt.Add(-time.Duration(a.AgeSeconds * float64(time.Second))).UTC()
	result.ValidForSeconds = math.Min(a.ValidForSeconds, (2 * constants.HealthCheckInterval).Seconds())
	if a.Stale {
		result.ValidForSeconds = 0
	}
	return &result
}

// SummarizeActivity combines positive evidence without treating unknowns as idle.
func SummarizeActivity(values []Activity, now time.Time) Activity {
	result := Activity{State: "idle", ObservedAt: now.UTC(), ValidForSeconds: (2 * constants.HealthCheckInterval).Seconds()}
	unknown := false
	for _, value := range values {
		current := value.Current(now, false)
		if current.State == "unknown" {
			unknown = true
			continue
		}
		result.ActiveRequests += current.ActiveRequests
		result.BusyEngines += current.BusyEngines
		result.ValidForSeconds = math.Min(result.ValidForSeconds, current.ValidForSeconds)
		if current.ObservedAt.Before(result.ObservedAt) {
			result.ObservedAt = current.ObservedAt
		}
	}
	if result.ActiveRequests > 0 || result.BusyEngines > 0 {
		result.State = "active"
	} else if unknown {
		result.State = "unknown"
		result.ValidForSeconds = 0
	}
	result.AgeSeconds = math.Max(0, now.Sub(result.ObservedAt).Seconds())
	return result
}
