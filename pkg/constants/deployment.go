package constants

import "time"

// Status is the lifecycle state of a deployment or of a single node within
// one. The superset covers both because 4 of 6 values overlap and a separate
// node-only type adds no type-safety (both are string-backed). Callers
// enforce which subset applies in their context:
//
//   - Deployment aggregate: Pending, Downloading, Completed, Failed, Cancelled
//   - Per-node slice:       Pending, Downloading, Completed, Failed, Skipped
//
// Cancelled applies only to the deployment as a whole (a user cancel stops
// every node). Skipped applies only to a node (the node was incompatible so
// the aggregate can still complete).
type Status string

// Status constants. Keep in sync with the OpenAPI enums for
// `Deployment.status` and `DeploymentNode.status` at
// internal/server/openapi.yaml.
const (
	StatusPending     Status = "pending"
	StatusDownloading Status = "downloading"
	StatusCompleted   Status = "completed"
	StatusFailed      Status = "failed"
	StatusCancelled   Status = "cancelled" // deployment-level only
	StatusSkipped     Status = "skipped"   // node-level only (incompatible)
)

// IsTerminal reports whether the value represents a final state in either
// context — the three values (Completed, Failed, Cancelled/Skipped) that
// stop further progress.
func (s Status) IsTerminal() bool {
	return s == StatusCompleted ||
		s == StatusFailed ||
		s == StatusCancelled ||
		s == StatusSkipped
}

// QueueUntilCallerDeadline is the queue_timeout value meaning "hold the
// caller until its own request context ends" rather than "wait this long".
// It is the default when a provider declares no queue_timeout: a single-slot
// engine has nothing to gain from failing a caller that is still waiting,
// and the caller's deadline is the only bound that reflects what the caller
// will actually accept. A provider that wants fail-fast writes "0s".
const QueueUntilCallerDeadline = time.Duration(-1)
