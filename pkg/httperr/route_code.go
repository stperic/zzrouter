package httperr

// RouteErrorCode is the closed-enum code set for cluster routing and job lookup failures.
//
// The enum exists so a routing failure stays sanitized (no raw Go error
// text reaches the client) while still being actionable: callers branch on
// Code instead of pattern-matching a prose Detail string.
type RouteErrorCode string

const (
	// CodeModelInUse refuses physical deletion while a run holds the weights.
	CodeModelInUse RouteErrorCode = "model_in_use"
	// CodeModelReferenced refuses weights deletion while variants reference them.
	CodeModelReferenced RouteErrorCode = "model_referenced"
	// CodeModelNotDeletable identifies an addressed route or logical variant.
	CodeModelNotDeletable       RouteErrorCode = "model_not_deletable"
	CodeCapacityExceeded        RouteErrorCode = "capacity_exceeded"
	CodeQueueWaitExpired        RouteErrorCode = "queue_wait_expired"
	CodeReleaseManifestMissing  RouteErrorCode = "release_manifest_missing"
	CodeProtocolCutoverRequired RouteErrorCode = "protocol_cutover_required"
	// CodeJobNotFound means confirmed absence within the requested lookup scope.
	CodeJobNotFound RouteErrorCode = "job_not_found"

	// CodeModelNodeAmbiguous requires an explicit node for replicated remote models.
	CodeModelNodeAmbiguous RouteErrorCode = "model_node_ambiguous"

	// CodeModelSourceAmbiguous requires an exact source id for same-name physical weights.
	CodeModelSourceAmbiguous RouteErrorCode = "model_source_ambiguous"
	// CodeModelDetailUnsupported means catalog identity exists without a supported detail reader.
	CodeModelDetailUnsupported RouteErrorCode = "model_detail_unsupported"

	// CodeNodeUnreachable is a transport failure against a named node: the
	// node is offline, unpaired, or its cluster certificate is not valid. Job
	// lookup also uses it when a peer that may own the job did not answer.
	CodeNodeUnreachable RouteErrorCode = "node_unreachable"

	// CodeClusterUnavailable is a broadcast with no reachable peer, or a
	// coordinator with no cluster client configured.
	CodeClusterUnavailable RouteErrorCode = "cluster_unavailable"

	// CodeUpstreamTimeout is a node that accepted the request but did not
	// answer before the request deadline.
	CodeUpstreamTimeout RouteErrorCode = "upstream_timeout"

	// CodeUpstreamCanceled is a caller-side cancellation (client hung up)
	// observed while the routed request was in flight.
	CodeUpstreamCanceled RouteErrorCode = "upstream_canceled"

	// CodeUpstreamShape is a successful transport whose body did not match
	// the shape this endpoint expected, which signals peer version skew.
	CodeUpstreamShape RouteErrorCode = "upstream_shape"

	CodeUpstreamStreamInterrupted RouteErrorCode = "upstream_stream_interrupted"
)
