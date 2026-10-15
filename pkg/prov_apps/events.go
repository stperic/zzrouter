package prov_apps

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	obsruns "github.com/stperic/zzrouter/pkg/observability/runs"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
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

// runEvent is an event of type t about run.
func runEvent(t EventType, run *instance.Instance, message string) Event {
	return Event{Type: t, Provider: run.Provider, Instance: run.ID, Model: run.Model, Message: message, Time: utils.Now()}
}

// prepareRecord opens/rotates outside the instance lock: logging may call user handlers.
// Only append/close and audit enqueue belong in the running commit.
func (m *ProviderAppManager) prepareRecord(run *instance.Instance, e Event) (commit, finish func()) {
	var f *os.File
	var err error
	if run.LogFilePath != "" {
		f, _, err = m.runLogFile(run)
	}
	return func() {
			if f != nil {
				_, err = fmt.Fprintf(f, "%s %s\n", e.Time.Format(time.RFC3339), e.Message)
				err = errors.Join(err, f.Close())
				f = nil
			}
			m.enqueueEvent(e)
		}, func() {
			if f != nil {
				err = errors.Join(err, f.Close())
			}
			if err != nil {
				slog.Warn("Failed to write a run's log", "run", run.ID, "error", err)
			}
		}
}

// record commits a log line and audit event before notifying catalog observers.
func (m *ProviderAppManager) record(run *instance.Instance, e Event) {
	commit, finish := m.prepareRecord(run, e)
	commit()
	finish()
	m.notifyCatalog(e)
}

// publishRunning commits lifecycle history with readiness, then permits reentrant observers.
func (m *ProviderAppManager) publishRunning(run *instance.Instance, stampActivity bool, message string) bool {
	event := runEvent(EventInstanceRunning, run, message)
	commit, finish := m.prepareRecord(run, event)
	observe := false
	accepted := run.MarkRunning(stampActivity, func(first bool) {
		commit()
		observe = first && m.providerSchema != nil && run.Port > 0 && run.Model != ""
		if observe {
			run.TrackGoroutine()
		}
	})
	if observe {
		go func() {
			defer run.GoroutineDone()
			defer utils.RecoverAndLog("prov_apps.readServedContext")
			m.readServedContext(run)
		}()
	}
	finish()
	if accepted {
		m.notifyCatalog(event)
	}
	return accepted
}

// failedEvent is run's failure for cause, reported as err.
func failedEvent(run *instance.Instance, cause obsruns.FailureCause, err error) Event {
	event := runEvent(EventInstanceFailed, run, err.Error())
	event.Error, event.Cause = err, cause
	return event
}

// stoppedEvent is run's stop for reason.
func stoppedEvent(run *instance.Instance, reason obsruns.StopReason, message string) Event {
	event := runEvent(EventInstanceStopped, run, message)
	event.Reason = reason
	return event
}
