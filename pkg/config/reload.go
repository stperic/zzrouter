package config

import (
	"fmt"
	"log/slog"
	"strings"
)

// ReloadDisposition is returned by every config-store OnChange listener.
// See docs/plan_reload_semantics.md for the full contract.
type ReloadDisposition int

const (
	// DispositionApplied — listener observed a material change and
	// reconciled its state to the new config.
	DispositionApplied ReloadDisposition = iota

	// DispositionIgnored — nothing this listener cares about actually
	// changed. Legitimate no-op, not an error.
	DispositionIgnored

	// DispositionRequiresRestart — a field this listener owns changed,
	// but hot-reload is not supported. The old runtime value is retained.
	DispositionRequiresRestart

	// DispositionRejected — the new value is structurally invalid for
	// this listener. Logged and ignored; the listener keeps running on
	// its previous value.
	DispositionRejected
)

func (d ReloadDisposition) String() string {
	switch d {
	case DispositionApplied:
		return "applied"
	case DispositionIgnored:
		return "ignored"
	case DispositionRequiresRestart:
		return "requires_restart"
	case DispositionRejected:
		return "rejected"
	default:
		return fmt.Sprintf("unknown(%d)", int(d))
	}
}

// ReloadEntry is one listener's contribution to the report.
type ReloadEntry struct {
	Listener    string
	Disposition ReloadDisposition
	Message     string // optional — populated on RequiresRestart / Rejected
}

// ReloadReport aggregates every listener's disposition from a single
// notify cycle. The store logs a one-line summary at the notify
// boundary; callers that expose reload via an HTTP handler render the
// full report for operators.
type ReloadReport struct {
	Entries []ReloadEntry
}

// Add appends a listener's disposition. Thread-unsafe — the store
// serializes notify, so listeners are invoked sequentially.
//
// ReloadEntry.Message stays blank at this signature; if and when a
// future listener contract threads a detail string (e.g., a
// DispositionRejected reason), add an overload or a fluent variant
// rather than a trailing positional string that's empty for every
// current call site.
func (r *ReloadReport) Add(listener string, disp ReloadDisposition) {
	r.Entries = append(r.Entries, ReloadEntry{
		Listener:    listener,
		Disposition: disp,
	})
}

// NeedsRestart reports whether any listener flagged a field it owns as
// RequiresRestart. Intended for the reload HTTP handler to decide
// whether to surface a 200 with warnings or a 409 with actionable
// details.
func (r *ReloadReport) NeedsRestart() bool {
	for _, e := range r.Entries {
		if e.Disposition == DispositionRequiresRestart {
			return true
		}
	}
	return false
}

// HasRejection reports whether any listener rejected the change. A true
// here means memory/disk are out of sync and operator action is needed.
func (r *ReloadReport) HasRejection() bool {
	for _, e := range r.Entries {
		if e.Disposition == DispositionRejected {
			return true
		}
	}
	return false
}

// LogSummary emits a one-line slog summary at the level dictated by the
// most severe disposition in the report. Called by stores at the end of
// notify() so ops always get a signal without needing to wire through
// every call site.
func (r *ReloadReport) LogSummary(source string) {
	if len(r.Entries) == 0 {
		return
	}
	counts := make(map[ReloadDisposition]int, 4)
	names := make([]string, 0, len(r.Entries))
	for _, e := range r.Entries {
		counts[e.Disposition]++
		names = append(names, e.Listener+"="+e.Disposition.String())
	}

	msg := fmt.Sprintf("Config reload (%s): %s", source, strings.Join(names, ", "))
	switch {
	case r.HasRejection():
		slog.Error(msg, "rejected", counts[DispositionRejected])
	case r.NeedsRestart():
		slog.Warn(msg, "requires_restart", counts[DispositionRequiresRestart])
	default:
		slog.Info(msg,
			"applied", counts[DispositionApplied],
			"ignored", counts[DispositionIgnored])
	}
}
