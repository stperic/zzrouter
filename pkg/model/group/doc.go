// Package group owns cluster-wide model groups — named collections of
// models/providers that route together (round-robin, latency-ranked,
// etc.).
//
// Primary type: [GroupStore] — constructed via [NewGroupStore]; holds
// the group catalog and applies [StrategyType]-based selection through
// the [GroupReader] interface.
//
// # Scope
//
// Group membership and strategy metadata only. Actual request
// dispatch lives in `pkg/cluster/mesh/dispatcher.go`; fallback
// decisions live in `pkg/fallback`.
//
// # Architecture
//
// See docs/package_architecture.md. One mutex on GroupStore; no
// ordering concern — no cross-subpackage lock reach.
package group
