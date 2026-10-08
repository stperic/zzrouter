// Package harness is the reusable e2e test library for zzrouter. It is
// used by the ring1_contract / ring2_inference / ring2_admin suites
// under test/e2e/ to drive a real running zzrouter cluster. Future
// chaos / soak / sdk_compat suites slot in alongside them.
//
// # Hard rule
//
// The harness package itself MUST NOT import "testing". Helpers return
// errors. Tests that want *testing.T-fatal behavior import the sibling
// package test/e2e/harness/assert which provides one-line shims
// (MustOK, MustProvision, JobOK, ...).
//
// This rule keeps the harness usable from the future CLI sweep tool,
// ad-hoc scripts, and chaos drivers that have no testing.T context.
//
// # Topology
//
// A Cluster has exactly one Coordinator and zero-or-more Workers. The
// concrete Provisioner brings them up; the rest of the harness API
// is backend-agnostic. Provisioners register themselves at init time
// via RegisterBackend so a profile can name a Backend tag without an
// explicit import in the test file (blank import on the sub-package
// is enough). Today's backends:
//
//   - "" (empty)     — externalBackend, dial addresses already running
//   - "inproc"       — test/e2e/harness/inproc, real *server.Server in-process
//   - "ssh"          — test/e2e/harness/ssh, dial pre-running zzrouter-node
//     processes; SSH used only for pairing automation
//     and failure-time log fetch
//   - "docker"       — (future) docker-compose
//
// # Typical flow
//
//	cfg := harness.Local()                  // or Nightly() / Full()
//	cluster, _ := harness.New(cfg)
//	_ = cluster.Provision(ctx)
//	defer cluster.Teardown(ctx)
//
//	coord := cluster.Coordinator()
//	c := harness.NewClient(coord, harness.TierAdmin)
//	resp, _ := c.GET(ctx, "/zzrouter/v1/server/version")
//
//	jobs := harness.NewJobs(c, 0)
//	out, _ := jobs.Wait(ctx, jobID)
//	if !out.Succeeded() { ... }
//
// # Files
//
//   - config.go        Config struct, Load, MustLoad, lookup helpers
//   - profile.go       Base/Local/Nightly/Full builders (code, not YAML)
//   - cluster.go       Cluster + Node + Provisioner interface
//   - backend.go       Backend registry + externalBackend default
//   - httpc.go         Client + ReqOpts + Response + Problem
//   - sse.go           TailSSE parser
//   - inference_stream.go  chat/embed/rerank streaming consumers
//   - inference.go     non-streaming inference helpers
//   - jobs.go          Jobs.Wait — SSE-tail with eviction-window fallback
//   - poll.go          PollUntil / WaitDeploymentReady
//   - routes.go        RouteSpec + FetchRoutes
//   - types.go         shared enums (Backend, Role, ModelRole, CloudMode)
//   - cassettes.go     go-vcr-style cloud egress recorder
//   - state.go         server-mediated snapshot/restore client
//   - diagnostics.go   failure-tarball collector
//   - deployments.go   deploy + WaitDeploymentReady client
//   - runs.go          /zzrouter/v1/runs + LaunchRun
//   - providers.go     /zzrouter/v1/providers/* helpers
//   - keys.go          /zzrouter/v1/keys + virtual-key auth
//   - nodes.go         /zzrouter/v1/nodes + RequireNodes
//   - openrouter_free.go  ListOpenRouterFreeModels (cloud-chat helper)
//
// # Auth
//
// Three static tiers (TierNone / TierAPI / TierAdmin / TierCluster)
// pick the X-API-Key value sent on every request. Per-request
// VirtualKey(s) opt overrides the static tier. The Node carries the
// keys; the Backend's Provisioner is responsible for sourcing them
// (env, file, or hardcoded test constants).
package harness
