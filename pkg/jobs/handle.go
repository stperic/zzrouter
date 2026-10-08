package jobs

import "context"

// Handle is the producer-side API. Returned by Registry.Start and
// used by the job goroutine to report progress and terminate. All
// methods are safe to call from any goroutine; late calls after
// Done/Fail are no-ops (a producer racing its own defer-Fail path
// must not panic).
type Handle interface {
	// Progress emits a running-phase event. Percent is clamped to
	// [0,100]; step and bytes are optional (pass empty string / zero
	// Bytes to omit). Non-blocking — slow subscribers lose oldest
	// events, they never stall the producer.
	Progress(percent int, step string, bytes Bytes)

	// Meta patches the sidecar payload. New keys are merged into the
	// job's current meta; existing keys are overwritten. The next
	// emitted event carries the merged map. Useful for late-arriving
	// context (e.g. resolved model filename on a download). A string
	// warning is also emitted as the event's top-level warning.
	Meta(m Meta)

	// Done marks the job complete. Emits a final done-phase event and
	// closes the handle to further Progress calls.
	Done()

	// Fail marks the job failed. err.Error() is stamped onto the final
	// event's Err field. Nil err is treated as an empty message.
	Fail(err error)

	// Context is cancelled when the job's lifecycle context terminates
	// (Registry.Stop, cancel signal, or janitor reap). Producers
	// should select on this to abort work promptly.
	Context() context.Context

	// ID returns the kind-prefixed job ID.
	ID() string

	// Epoch returns the per-job nonce used for ?from=<seq>&epoch= resume
	// disambiguation.
	Epoch() string
}
