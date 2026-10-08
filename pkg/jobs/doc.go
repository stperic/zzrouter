// Package jobs is the shared progress + streaming subsystem. Every
// zzrouter node runs its own *Registry; long-running operations
// (downloads, provider installs, updates, sync, inference-log) call
// Registry.Start to open a *Handle, then Progress / Done / Fail during
// the operation. Subscribers open a Subscriber on the registry to
// receive a live stream of Events; SSE transport layers on top.
//
// The package is role-agnostic: every node owns its local producer
// registry regardless of cluster role (see pkg/cluster/role for the
// node-vs-role invariant). HTTP wiring — public jobs endpoints on
// coord, internal endpoints on every cluster-paired node — lives in
// internal/server; the package here has no cluster or HTTP surface.
//
// Ring policy is per-kind and tri-state: Bounded (typical jobs,
// N-event ring with replay), Firehose (high-rate non-terminal streams
// like inference-log — no ring, channel-only, no replay), Terminal
// (short jobs, small ring). The Kind's default policy is returned by
// Kind.DefaultRingPolicy; Config.KindPolicy overrides per-registry.
//
// Lifecycle: pending → running → done|failed. Late Progress calls after
// a terminal transition are no-ops (the producer may lose a race with
// its own defer-Fail path). An inactivity-gated janitor flips silently
// stalled jobs to failed after the kind's idle timeout, bounding memory.
//
// Subscribe-after-terminal contract (load-bearing for clients that
// consume /jobs/:id/stream): for a terminal job still in the registry
// (within CompletedTTL), Subscribe replays the entire ring under j.mu
// and closes the subscriber channel — the terminal event is the last
// ring entry, so the reader always observes done|failed|cancelled
// before end-of-stream. There is no race between the producer's
// terminal transition and a late subscriber within that window.
//
// The eviction boundary is the only cliff: the janitor's
// delete(reg.jobs, id) runs outside the per-job lock, so a Subscribe
// attempt that races eviction returns ErrNotFound (the SSE handler
// 404s). Clients must treat 404 on /jobs/:id/stream as "either never
// existed or aged out" — distinguish via the prior /jobs/:id 200 if
// the lifecycle requires it. Default e2e harness behavior: SSE-tail,
// fall back to a single Get on close-without-terminal, surface
// ErrJobEvicted on 404.
package jobs
