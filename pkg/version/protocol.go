package version

// Cluster protocol version constants.
//
// These define the cluster-internal wire contract — pairing handshake shape,
// /zzrouter/v1/internal/* route payloads, mesh heartbeat/health payloads.
// Bumped independently from marketing semver so patch/minor releases don't
// fragment clusters, and so wire-breaking changes have an explicit signal.
//
// When bumping:
//   - Wire change that old peers can safely ignore (new optional field):
//     bump ClusterProtocolVersion only. MinClusterProtocolVersion stays on
//     the previous value for one release so rolling upgrades work.
//   - Wire change that old peers would mis-parse (reshaped/removed field,
//     new required field): bump BOTH. Operators must upgrade all peers
//     together.
//   - Advance MinClusterProtocolVersion in the release AFTER the compat
//     window closes, not the same release that introduces the break.
//
// Build version (Major.Minor.Patch) is diagnostic only and never gates.
//
// Protocol 1 is the first public release's wire: pairing, internal routes,
// provider sync with assets, model defaults, variants and features, typed
// install recipes, and the cluster update routes.
// Protocol 2 adds failure_patterns, which older provider schemas reject.
// Protocol 3 adds readiness_probe.stall_timeout, which older provider schemas reject.
// Protocol 4 preserves selected provider/source identity and compact metadata on internal model show.
// Protocol 5 requires guarded exact model deletion and physical run reservations.
// Protocol 6 requires tier-resolved idle and memory eviction run settings.
// Protocol 7 requires observable placement jobs and terminal transfer evidence.
// Protocol 8 requires node-scoped runs, activity probes and memory commitments.
// Protocol 9 requires provider data/model stores and owned download targets.
// Protocol 10 adds release-owned source install recipes, which older provider schemas reject.
// Protocol 11 adds endpoint features and feature health checks, which older provider schemas reject.
// Protocol 12 adds tools evidence declarations, which older provider schemas reject, and optional
// node and preview capacity assessments.
// Protocol 13 adds context-aware memory evidence; legacy assessments degrade to unknown.
const (
	ClusterProtocolVersion    = 13
	MinClusterProtocolVersion = 12
)

// ProtocolWindow describes a coordinator's accepted cluster protocol range.
// Use LocalProtocolWindow() to get the current build's window.
type ProtocolWindow struct {
	Min int // oldest peer protocol accepted
	Max int // newest peer protocol understood
}

// LocalProtocolWindow returns the current build's accepted protocol range.
func LocalProtocolWindow() ProtocolWindow {
	return ProtocolWindow{Min: MinClusterProtocolVersion, Max: ClusterProtocolVersion}
}

// Check validates a peer's advertised cluster_protocol against this window.
// Coordinator-side only; workers advertise but do not gate the coordinator.
//
// Rejects in three cases (cap both ends):
//   - remote == 0: peer predates protocol advertisement; no silent accept.
//   - remote < Min: peer too old; operator should upgrade the peer.
//   - remote > Max: peer too new; coordinator may mis-parse reshaped
//     fields. Operator should upgrade the coordinator.
//
// No dev-build bypass: ClusterProtocolVersion is a source-level const and
// is populated in every build, including plain `go build` without ldflags.
func (w ProtocolWindow) Check(remote int) error {
	return CheckClusterProtocol(w.Max, w.Min, remote)
}
