// Package steering parses per-call X-Route-* headers into a structured
// Hints value used by the dispatch chain to bias replica selection.
//
// Tier 0.5 of the routes-agent-control-api plan. Two hints ship in this
// phase; the remaining four are rejected at parse time with a closed-
// enum error until their dependent engine arc lands:
//
//	X-Route-Exclude-Replicas    ✅  hard filter
//	X-Route-Latency-Budget-Ms   ✅  hard filter
//	X-Route-Require-Tags        ✅  hard filter (renamed from X-Route-Tags)
//	X-Route-Require-Capabilities ❌ needs Replica.Capabilities (Phase 8)
//	X-Route-Max-Cost-Micro       ❌ needs predictive cost (Phase 6 preview)
//	X-Route-Prefer-Tags          ❌ needs strategy ctx widening (Phase 8)
//	X-Route-Prefer-Model         ❌ needs strategy ctx widening (Phase 8)
//
// One canonical hard-filter-on-tags header: X-Route-Require-Tags. The
// previous X-Route-Tags has been retired (greenfield rename).
package steering

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// HeaderGetter is the minimal contract steering needs from the HTTP
// layer. Keeps this package free of gin / net/http imports so it can
// be exercised by table-driven tests without an HTTP harness.
type HeaderGetter interface {
	GetHeader(name string) string
}

// Hints carries the parsed per-call steering. Zero value is "no hints
// set" and every consumer must treat it as a clean no-op — don't add
// fields with a non-zero "off" state.
type Hints struct {
	// ExcludeReplicas hard-filters replicas by name. Capped at
	// maxExcludeReplicas; anything longer is rejected at parse time.
	ExcludeReplicas []string

	// LatencyBudgetMs caps the per-replica AverageLatency the dispatch
	// chain will tolerate. Zero means "no budget set". Replicas with
	// no latency samples (HasData=false) pass — fail-open on cold-start
	// replicas, otherwise the budget creates a permanent dead state
	// where untried replicas can never accumulate evidence.
	LatencyBudgetMs int

	// RequireTags is the hard tag filter (renamed from X-Route-Tags).
	// A replica passes only if its tag set is a superset of RequireTags.
	RequireTags []string
}

// ParamName is the closed-enum echo key written into
// RoutingMetadata.SteeringApplied. Maps 1:1 to the future POST /preview
// body field, per plan §"Tier 0.5". Don't use header names here —
// agents pivot on these strings.
type ParamName string

const (
	ParamExcludeReplicas ParamName = "exclude_replicas"
	ParamLatencyBudgetMs ParamName = "latency_budget_ms"
	ParamRequireTags     ParamName = "require_tags"
)

// HeaderExcludeReplicas et al are the canonical MIME-style header
// names. Gin canonicalises on lookup, so callers using lowercased
// variants work too — the constants exist so the wire vocabulary is
// listed in one place.
const (
	HeaderExcludeReplicas = "X-Route-Exclude-Replicas"
	HeaderLatencyBudgetMs = "X-Route-Latency-Budget-Ms"
	HeaderRequireTags     = "X-Route-Require-Tags"

	// Deferred headers — rejected at parse time with ErrUnsupportedHeader
	// so an agent gets a closed-enum 400 instead of silent acceptance.
	headerRequireCapabilities = "X-Route-Require-Capabilities"
	headerMaxCostMicro        = "X-Route-Max-Cost-Micro"
	headerPreferTags          = "X-Route-Prefer-Tags"
	headerPreferModel         = "X-Route-Prefer-Model"
)

// maxExcludeReplicas bounds the per-request exclude list so an agent
// can't ship a 10k-name header and force a linear scan per candidate.
// 100 is the natural ceiling — you can't exclude more than the
// candidate set, and real fleets carry far fewer.
const maxExcludeReplicas = 100

// maxReplicaNameLen caps each name in Exclude-Replicas. Replica names
// are user-defined and live in YAML; 256 bytes is plenty and rejects
// pathological inputs.
const maxReplicaNameLen = 256

// maxLatencyBudgetMs is a sanity bound on the budget header. An hour
// is well past anything useful; values above it are likely an agent
// bug (negative cast to uint, etc.).
const maxLatencyBudgetMs = 3_600_000

// ErrUnsupportedHeader signals a Tier 0.5 header that hasn't shipped
// yet. The caller surfaces it as a 400 + closed-enum error code so an
// agent can distinguish "header not understood" from "header invalid".
var ErrUnsupportedHeader = errors.New("steering header not supported in this build")

// ParseError carries the offending header + a human message. Returned
// from Parse so the HTTP layer can build a closed-enum error body.
type ParseError struct {
	Header  string
	Message string
	Err     error
}

func (e *ParseError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("invalid %s: %s", e.Header, e.Message)
}

func (e *ParseError) Unwrap() error { return e.Err }

// Parse reads the supported X-Route-* headers off g and returns the
// resulting Hints. Returns a *ParseError on the first validation
// failure. Unsupported (deferred) headers fail fast with
// ErrUnsupportedHeader wrapping the header name.
func Parse(g HeaderGetter) (Hints, *ParseError) {
	for _, h := range []string{headerRequireCapabilities, headerMaxCostMicro, headerPreferTags, headerPreferModel} {
		if g.GetHeader(h) != "" {
			return Hints{}, &ParseError{
				Header:  h,
				Message: "header is reserved for a later phase of the routes agent-control plan",
				Err:     ErrUnsupportedHeader,
			}
		}
	}

	var h Hints
	if raw := g.GetHeader(HeaderExcludeReplicas); raw != "" {
		names, err := parseNameList(raw)
		if err != nil {
			return Hints{}, &ParseError{Header: HeaderExcludeReplicas, Message: err.Error(), Err: err}
		}
		h.ExcludeReplicas = names
	}
	if raw := g.GetHeader(HeaderLatencyBudgetMs); raw != "" {
		ms, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return Hints{}, &ParseError{Header: HeaderLatencyBudgetMs, Message: "must be a positive integer (milliseconds)", Err: err}
		}
		if ms <= 0 || ms > maxLatencyBudgetMs {
			return Hints{}, &ParseError{Header: HeaderLatencyBudgetMs, Message: fmt.Sprintf("must be in [1, %d]", maxLatencyBudgetMs)}
		}
		h.LatencyBudgetMs = ms
	}
	if raw := g.GetHeader(HeaderRequireTags); raw != "" {
		tags, err := parseNameList(raw)
		if err != nil {
			return Hints{}, &ParseError{Header: HeaderRequireTags, Message: err.Error(), Err: err}
		}
		h.RequireTags = tags
	}
	return h, nil
}

// parseNameList splits a comma-separated header value into a slice of
// trimmed non-empty tokens, validating each against length + control-
// char rules. The same shape backs Exclude-Replicas and Require-Tags;
// keeping one helper avoids divergent validation drift.
func parseNameList(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	if len(parts) > maxExcludeReplicas {
		return nil, fmt.Errorf("too many values (max %d)", maxExcludeReplicas)
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" {
			continue
		}
		if len(t) > maxReplicaNameLen {
			return nil, fmt.Errorf("value %q exceeds %d bytes", t[:32]+"…", maxReplicaNameLen)
		}
		for _, r := range t {
			if r < 0x20 || r == 0x7f {
				return nil, fmt.Errorf("value contains a control character (U+%04X)", r)
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// HasAny reports whether any hint is set. Used by the wire layer to
// decide whether to emit SteeringApplied at all.
func (h Hints) HasAny() bool {
	return len(h.ExcludeReplicas) > 0 || h.LatencyBudgetMs > 0 || len(h.RequireTags) > 0
}

// Applied returns the parameter-name keys for every hint actually
// populated on h, sorted for deterministic wire output. Used by the
// commit path to fill RoutingMetadata.SteeringApplied.
func (h Hints) Applied() []string {
	var out []string
	if len(h.ExcludeReplicas) > 0 {
		out = append(out, string(ParamExcludeReplicas))
	}
	if h.LatencyBudgetMs > 0 {
		out = append(out, string(ParamLatencyBudgetMs))
	}
	if len(h.RequireTags) > 0 {
		out = append(out, string(ParamRequireTags))
	}
	return out
}
