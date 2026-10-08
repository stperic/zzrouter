package jobs

import "time"

// Kind is the operation category a job represents. Ring policy, ID
// prefix, and inactivity timeouts are derived from the kind.
type Kind string

const (
	KindDownload     Kind = "download"
	KindInstall      Kind = "install"
	KindUpdate       Kind = "update"
	KindSync         Kind = "sync"
	KindInferenceLog Kind = "inference_log"
	KindRun          Kind = "run"
)

// RingMode selects the event-retention strategy for a kind.
type RingMode int

const (
	// RingBounded: fixed-size ring supporting ?from=<seq> replay.
	// Overflow emits a single events_dropped marker.
	RingBounded RingMode = iota

	// RingFirehose: no ring, channel-only. ?from=<seq> is rejected;
	// only ?from=now is valid. For high-rate non-terminal streams
	// (inference-log) where replay isn't meaningful and a large ring
	// would waste memory on every subscriber.
	RingFirehose

	// RingTerminal: small ring for short-lived jobs (few events total).
	RingTerminal
)

// RingPolicy bundles mode + size. Size semantics vary by mode:
//
//   - Bounded / Terminal: ring slot count.
//   - Firehose: per-subscriber channel buffer.
type RingPolicy struct {
	Mode RingMode
	Size int
}

// Kind-prefixed job IDs make `grep dl_` and similar one-liners useful
// against structured logs. Prefix is auto-derived in Registry.Start —
// producers cannot mis-tag.
func (k Kind) idPrefix() string {
	switch k {
	case KindDownload:
		return "dl_"
	case KindInstall:
		return "inst_"
	case KindUpdate:
		return "upd_"
	case KindSync:
		return "sync_"
	case KindInferenceLog:
		return "inflog_"
	case KindRun:
		return "run_"
	}
	return "job_"
}

// Ring sizes picked from the plan doc (plan_jobs.md §Ring policy):
// downloads emit one event per chunk over seconds-to-minutes so 200 holds
// a useful replay window; installs and updates emit fewer, staged events
// so 100 is enough; sync is short so 50; inference-log is a firehose,
// channel buffer of 256 matches the existing inference-log subscriber.
const (
	defaultRingDownload     = 200
	defaultRingInstall      = 100
	defaultRingUpdate       = 100
	defaultRingSync         = 50
	defaultRingInferenceLog = 256
	// Run events: opening "launching" + per-500ms status polls are
	// negligible, but the 30s heartbeat accumulates ~2/minute. 300
	// comfortably covers a 2.5-hour outlier load before dropping
	// oldest. Real loads are minutes, so 300 is headroom, not a ceiling.
	defaultRingRun      = 300
	defaultRingFallback = 100
)

// DefaultRingPolicy returns the ring policy for a kind. Callers can
// override via Config.KindPolicy keyed on Kind.
func (k Kind) DefaultRingPolicy() RingPolicy {
	switch k {
	case KindDownload:
		return RingPolicy{Mode: RingBounded, Size: defaultRingDownload}
	case KindInstall:
		return RingPolicy{Mode: RingBounded, Size: defaultRingInstall}
	case KindUpdate:
		return RingPolicy{Mode: RingBounded, Size: defaultRingUpdate}
	case KindSync:
		return RingPolicy{Mode: RingTerminal, Size: defaultRingSync}
	case KindInferenceLog:
		return RingPolicy{Mode: RingFirehose, Size: defaultRingInferenceLog}
	case KindRun:
		return RingPolicy{Mode: RingBounded, Size: defaultRingRun}
	}
	return RingPolicy{Mode: RingBounded, Size: defaultRingFallback}
}

// Inactivity timeouts bound memory against stalled producers. A
// producer that keeps calling Progress is never flipped; a silent
// producer is. Values are ceilings for realistic operations on
// home-lab hardware + links.
const (
	inactivityDownload     = 1 * time.Hour
	inactivityInstall      = 30 * time.Minute
	inactivityUpdate       = 30 * time.Minute
	inactivitySync         = 10 * time.Minute
	inactivityInferenceLog = 0 // firehose has no idle concept; never reaped
	// Runs can take 30+ min for a 70B model on cold disk + CUDA init.
	// Watcher emits a 30s heartbeat so the janitor sees liveness, but
	// the ceiling is generous to cover outlier loads.
	inactivityRun      = 2 * time.Hour
	inactivityFallback = 30 * time.Minute
)

// DefaultInactivityTimeout returns the max-silent-stretch before the
// janitor flips a running job to failed. Zero means "never" (firehose).
func (k Kind) DefaultInactivityTimeout() time.Duration {
	switch k {
	case KindDownload:
		return inactivityDownload
	case KindInstall:
		return inactivityInstall
	case KindUpdate:
		return inactivityUpdate
	case KindSync:
		return inactivitySync
	case KindInferenceLog:
		return inactivityInferenceLog
	case KindRun:
		return inactivityRun
	}
	return inactivityFallback
}
