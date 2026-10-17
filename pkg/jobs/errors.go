package jobs

import "errors"

var (
	// ErrNotFound is returned by Registry.Get / Subscribe when no job
	// matches the given ID (or TTL evicted it).
	ErrNotFound = errors.New("jobs: not found")

	// ErrRegistryStopped is returned by Start / Subscribe after Stop.
	ErrRegistryStopped = errors.New("jobs: registry stopped")

	// ErrReplayUnsupported is returned by Subscribe when the caller
	// requests historical replay (From > 0) on a Firehose kind.
	ErrReplayUnsupported = errors.New("jobs: historical replay not supported on this kind")

	// ErrEpochMismatch is returned by Subscribe when the caller's Epoch
	// hint does not match the job's current epoch — the worker
	// restarted or the job was re-created under the same ID. Client
	// must resnapshot.
	ErrEpochMismatch = errors.New("jobs: epoch mismatch")

	// ErrCanceled is the cause of a job context Registry.Cancel ended; a
	// terminal transition, reap or registry stop ends it with another cause.
	ErrCanceled = errors.New("jobs: canceled")
)
