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

// DefaultQueueWait bounds how long a request waits for a slot when the provider sets no
// queue_timeout, so a caller with no deadline of its own cannot wait forever.
const DefaultQueueWait = 30 * time.Second

// DefaultQueueDepthFactor caps waiters at two full batches of the instance's concurrency;
// more would only wait out DefaultQueueWait and fail.
const DefaultQueueDepthFactor = 2

// QueueUntilCallerDeadline is queue_timeout "caller": wait as long as the caller's own context.
const QueueUntilCallerDeadline = time.Duration(-1)
