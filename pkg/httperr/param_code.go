package httperr

// ParamErrorCode is the closed-enum code set for provider-parameter
// validation failures (plan §7.2). Each value is stable on the wire.
// Adding a new failure mode is a code change, not a string-literal drift
// at call sites.
type ParamErrorCode string

const (
	CodeUnknownFlag ParamErrorCode = "unknown_flag"
	// CodeUnknownField is a STRUCTURAL failure: a key the merge-patch
	// body does not define, as opposed to CodeUnknownFlag's unknown
	// parameter name inside a block this endpoint does define.
	CodeUnknownField   ParamErrorCode = "unknown_field"
	CodeWrongType      ParamErrorCode = "wrong_type"
	CodeOutOfRange     ParamErrorCode = "out_of_range"
	CodeUnknownNode    ParamErrorCode = "unknown_node"
	CodeUnknownModel   ParamErrorCode = "unknown_model"
	CodeCoercionFailed ParamErrorCode = "coercion_failed"
	CodeRetired        ParamErrorCode = "retired"

	// Routes / model-groups (routes-agent-control-api).
	CodeRouteUnknown       ParamErrorCode = "route_unknown"
	CodeReplicaUnknown     ParamErrorCode = "replica_unknown"
	CodeReplicaLastInGroup ParamErrorCode = "replica_last_in_group"
	CodeRouteNotClaimed    ParamErrorCode = "route_not_claimed"

	// CodeReservedName rejects a user-chosen resource name that equals a
	// literal route segment sibling of the resource's :param route. gin
	// gives literals priority, so such a resource would be created but
	// unreachable by name.
	CodeReservedName ParamErrorCode = "reserved_name"

	// Request-body validation. These three arrived when the second error
	// envelope was retired: the body validator used to report an open
	// string (the go-playground tag that failed) in a field named `rule`,
	// which meant the surface had two vocabularies for one idea and only
	// one of them was closed.
	//
	// CodeRequired is a missing required input.
	CodeRequired ParamErrorCode = "required"
	// CodeInvalidValue is a value of the right type that is not
	// acceptable: outside an enum, malformed as an email or a URL, the
	// wrong length. Want carries the expectation.
	CodeInvalidValue ParamErrorCode = "invalid_value"
	// CodePreflightFailed is an install preflight check that did not
	// pass. Key is the check name rather than an input, and Hint is
	// usually the whole point of the entry.
	CodePreflightFailed ParamErrorCode = "preflight_failed"

	// CodeUnknownAsset is an asset-typed parameter naming a file the
	// provider does not have.
	CodeUnknownAsset ParamErrorCode = "unknown_asset"

	// CodeAssetInUse refuses removing an asset a parameter still names;
	// Key is that parameter's merge-patch path.
	CodeAssetInUse ParamErrorCode = "asset_in_use"
)

// The item and envelope that used to live here are gone. A per-key
// failure is a utils.ParamError inside the RFC 9457 problem envelope;
// this file keeps the vocabulary its Code field is drawn from.

// AllParamErrorCodes returns every declared ParamErrorCode.
//
// This exists so nothing has to hand-maintain a second copy of the
// list. A hand-written enumeration drifts silently and in the worst
// direction: the schema endpoint that advertises these codes to agents
// under-reports, so a client builds a local validator that treats a
// real code as unrecognised. That had already happened to two of them.
//
// Order is declaration order, which groups the parameter codes ahead of
// the routes ones; callers that need a stable presentation order should
// sort a copy.
//
// TestAllParamErrorCodesIsComplete parses this file and fails if a
// constant is declared without being listed here.
func AllParamErrorCodes() []ParamErrorCode {
	return []ParamErrorCode{
		CodeUnknownFlag,
		CodeUnknownField,
		CodeWrongType,
		CodeOutOfRange,
		CodeUnknownNode,
		CodeUnknownModel,
		CodeCoercionFailed,
		CodeRetired,
		CodeRouteUnknown,
		CodeReplicaUnknown,
		CodeReplicaLastInGroup,
		CodeRouteNotClaimed,
		CodeReservedName,
		CodeRequired,
		CodeInvalidValue,
		CodePreflightFailed,
		CodeUnknownAsset,
		CodeAssetInUse,
	}
}
