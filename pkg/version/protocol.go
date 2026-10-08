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
// Protocol 2: the internal probe route moved from
// /internal/instances/:id/probes to /internal/runs/:id/probes (API-shape
// arc 1.3). Old and new peers 404 each other's dispatch path, so both
// ends bump together; upgrade coordinator and workers in one flag day.
//
// Protocol 3: the provider sync push carries the provider's asset files,
// and schema.yaml may declare parameters of type asset. A protocol-2
// worker would drop both silently and hand an engine a bare file name as
// if it were a path, so the minimum moves with it: upgrade coordinator
// and workers together.
//
// Protocol 4: a synced provider config may carry model_defaults (the
// release's per-family tier) and models entries with from and request
// (variants), and schema.yaml may declare an asset with pass: content.
// A protocol-3 worker's config schema refuses the first two outright, and
// it would launch a variant's name as if it were weights and hand mlx a
// path where it takes a template, so the minimum moves with it: upgrade
// coordinator and workers together.
//
// Protocol 5: synced providers declare features. Protocol-4 workers decode
// configs with KnownFields(true) and refuse that block, so upgrade all
// coordinators and workers together.
// Protocol 6 adds optional version-only cluster update routes. Protocol-5
// peers stay connected but must be upgraded before receiving update commands.
// Protocol 7 adds strict synced typed install recipes and immutable runtime plans.
// Protocol-6 peers would reject or discard those keys. Upgrade every node together.
const (
	ClusterProtocolVersion    = 7
	MinClusterProtocolVersion = 7
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
