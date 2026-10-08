package prov_apps

import (
	"time"

	obsruns "github.com/stperic/zzrouter/pkg/observability/runs"
)

// EventType identifies the kind of lifecycle event.
type EventType int

const (
	// Instance lifecycle events.
	EventInstanceStarting EventType = iota
	EventInstanceRunning
	EventInstanceUnhealthy
	EventInstanceFailed
	EventInstanceStopping
	EventInstanceStopped

	// Install lifecycle events.
	EventInstallStarted
	EventInstallProgress
	EventInstallCompleted
	EventInstallFailed
	EventUpgradeStarted
	EventUpgradeCompleted
	EventUpgradeFailed
	EventUninstallCompleted
)

var eventTypeNames = [...]string{
	EventInstanceStarting:   "instance_starting",
	EventInstanceRunning:    "instance_running",
	EventInstanceUnhealthy:  "instance_unhealthy",
	EventInstanceFailed:     "instance_failed",
	EventInstanceStopping:   "instance_stopping",
	EventInstanceStopped:    "instance_stopped",
	EventInstallStarted:     "install_started",
	EventInstallProgress:    "install_progress",
	EventInstallCompleted:   "install_completed",
	EventInstallFailed:      "install_failed",
	EventUpgradeStarted:     "upgrade_started",
	EventUpgradeCompleted:   "upgrade_completed",
	EventUpgradeFailed:      "upgrade_failed",
	EventUninstallCompleted: "uninstall_completed",
}

// String returns a human-readable name for the event type.
func (e EventType) String() string {
	if int(e) < len(eventTypeNames) {
		return eventTypeNames[e]
	}
	return "unknown"
}

// Event represents a provider or instance lifecycle state change.
// Emitted by ProviderAppManager onto an internal audit channel and
// drained by eventLoop for structured logging.
//
// Error is for in-process use only and is not serialized to JSON.
// Use Message for the string representation when serializing.
//
// Reason and Cause carry the closed-enum classification consumed by
// the deployment-lifecycle metrics. They're populated only on the
// EventInstanceStopped / EventInstanceFailed types respectively;
// other event types leave them empty. Typed fields here (rather than
// substring-matching Message) make sentinel-renames a compile error
// instead of a silent metric regression.
type Event struct {
	Type     EventType            `json:"type"`
	Provider string               `json:"provider"`
	Instance string               `json:"instance,omitempty"` // Instance ID; empty for install events
	Model    string               `json:"model,omitempty"`
	Message  string               `json:"message,omitempty"`
	Error    error                `json:"-"` // In-process only; not serialized
	Reason   obsruns.StopReason   `json:"reason,omitempty"`
	Cause    obsruns.FailureCause `json:"cause,omitempty"`
	Time     time.Time            `json:"time"`
}
