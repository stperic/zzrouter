package inferencelog

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// Totals accumulates per-model usage for the lifetime of the process.
//
// It exists because the entry ring is a fixed-size buffer: summing the ring
// answers "over the last N requests", which silently under-reports once the
// buffer wraps, and on a busy node that happens in minutes. A running total
// is the only way to answer "since this node started" honestly, and it costs
// O(models) memory rather than O(requests).
type Totals struct {
	mu      sync.RWMutex
	since   time.Time
	byModel map[string]*modelTotals
	clock   func() time.Time
}

// modelTotals is the mutable accumulator behind one ModelUsage.
type modelTotals struct {
	provider  string
	model     string
	nodes     map[string]struct{}
	requests  int64
	errors    int64
	tokensIn  int64
	tokensOut int64

	costUSD float64
	// priced records whether ANY request carried an authoritative cost. It
	// is what separates "this model is free to run" from "we have no pricing
	// for it", which a bare 0.0 cannot express — and rendering an unpriced
	// self-hosted model as $0.00 reads as broken rather than as free.
	priced bool

	sumLatencyMs   float64
	peakTokensPerS float64
	firstSeen      time.Time
	lastSeen       time.Time
}

// ModelUsage is one model's running usage, as reported to callers.
type ModelUsage struct {
	Model    string   `json:"model"`
	Provider string   `json:"provider,omitempty"`
	Nodes    []string `json:"nodes,omitempty"`

	Requests  int64 `json:"requests"`
	Errors    int64 `json:"errors,omitempty"`
	TokensIn  int64 `json:"tokens_in"`
	TokensOut int64 `json:"tokens_out"`

	// CostUSD is populated only when Priced is true. Callers omit the cost
	// field entirely when it is not, rather than showing zero.
	CostUSD float64 `json:"cost_usd,omitempty"`
	Priced  bool    `json:"priced"`

	// AvgTokensPerSec is generation throughput: output tokens over the total
	// time spent generating them.
	AvgTokensPerSec  float64 `json:"avg_tokens_per_sec,omitempty"`
	PeakTokensPerSec float64 `json:"peak_tokens_per_sec,omitempty"`
	AvgLatencyMs     float64 `json:"avg_latency_ms,omitempty"`

	FirstSeen string `json:"first_seen,omitempty"`
	LastSeen  string `json:"last_seen,omitempty"`
}

// NewTotals returns an accumulator whose window opens now.
func NewTotals() *Totals {
	t := &Totals{
		byModel: make(map[string]*modelTotals),
		clock:   utils.Now,
	}
	t.since = t.clock()
	return t
}

// Since reports when accumulation began, which is process start.
func (t *Totals) Since() time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.since
}

// key identifies a model within a provider. The same model name under two
// providers has different economics and different hardware, so they are not
// summed together.
func totalsKey(provider, model string) string {
	return provider + "\x00" + model
}

// Record folds one entry into the running totals. Entries with no model are
// ignored: they cannot be attributed and would inflate a bucket nobody asked
// for.
func (t *Totals) Record(entry LogEntry) {
	model := strings.TrimSpace(entry.Model)
	if model == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	k := totalsKey(entry.App, model)
	m, ok := t.byModel[k]
	if !ok {
		m = &modelTotals{
			provider:  entry.App,
			model:     model,
			nodes:     make(map[string]struct{}),
			firstSeen: entry.Timestamp,
		}
		t.byModel[k] = m
	}

	m.requests++
	if entry.Status != "" && entry.Status != "success" && entry.Status != "ok" {
		m.errors++
	}
	m.tokensIn += entry.TokensIn
	m.tokensOut += entry.TokensOut
	m.sumLatencyMs += entry.LatencyMs
	if entry.TokensPerSec > m.peakTokensPerS {
		m.peakTokensPerS = entry.TokensPerSec
	}
	if entry.Node != "" {
		m.nodes[entry.Node] = struct{}{}
	}

	// Only an authoritative cost counts. CostSource is the closed-enum tag
	// set by the cost calculator; an empty one means no pricing was
	// available, which must not be recorded as a real zero.
	if entry.CostSource != "" {
		m.priced = true
		m.costUSD += entry.Cost
	}

	if m.firstSeen.IsZero() || (!entry.Timestamp.IsZero() && entry.Timestamp.Before(m.firstSeen)) {
		m.firstSeen = entry.Timestamp
	}
	if entry.Timestamp.After(m.lastSeen) {
		m.lastSeen = entry.Timestamp
	}
}

// snapshot converts an accumulator into its reported form.
func (m *modelTotals) snapshot() ModelUsage {
	out := ModelUsage{
		Model:            m.model,
		Provider:         m.provider,
		Requests:         m.requests,
		Errors:           m.errors,
		TokensIn:         m.tokensIn,
		TokensOut:        m.tokensOut,
		Priced:           m.priced,
		PeakTokensPerSec: m.peakTokensPerS,
	}
	if m.priced {
		out.CostUSD = m.costUSD
	}
	if m.sumLatencyMs > 0 {
		out.AvgTokensPerSec = float64(m.tokensOut) / (m.sumLatencyMs / 1000)
	}
	if m.requests > 0 {
		out.AvgLatencyMs = m.sumLatencyMs / float64(m.requests)
	}
	if !m.firstSeen.IsZero() {
		out.FirstSeen = m.firstSeen.UTC().Format(time.RFC3339)
	}
	if !m.lastSeen.IsZero() {
		out.LastSeen = m.lastSeen.UTC().Format(time.RFC3339)
	}

	out.Nodes = make([]string, 0, len(m.nodes))
	for n := range m.nodes {
		out.Nodes = append(out.Nodes, n)
	}
	sort.Strings(out.Nodes)

	return out
}

// All returns every model's usage, busiest first.
func (t *Totals) All() []ModelUsage {
	t.mu.RLock()
	defer t.mu.RUnlock()

	out := make([]ModelUsage, 0, len(t.byModel))
	for _, m := range t.byModel {
		out = append(out, m.snapshot())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// ForModel returns usage for one model. Provider may be empty to sum every
// provider serving that name, which is what a caller asking about "the model"
// rather than "this deployment" wants.
func (t *Totals) ForModel(provider, model string) (ModelUsage, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if provider != "" {
		m, ok := t.byModel[totalsKey(provider, model)]
		if !ok {
			return ModelUsage{}, false
		}
		return m.snapshot(), true
	}

	var matched []*modelTotals
	for _, m := range t.byModel {
		if strings.EqualFold(m.model, model) {
			matched = append(matched, m)
		}
	}
	if len(matched) == 0 {
		return ModelUsage{}, false
	}
	return mergeTotals(model, matched), true
}

// mergeTotals sums several providers' accumulators for one model name.
func mergeTotals(model string, parts []*modelTotals) ModelUsage {
	merged := &modelTotals{model: model, nodes: make(map[string]struct{})}
	providers := make([]string, 0, len(parts))

	for _, p := range parts {
		merged.requests += p.requests
		merged.errors += p.errors
		merged.tokensIn += p.tokensIn
		merged.tokensOut += p.tokensOut
		merged.sumLatencyMs += p.sumLatencyMs
		if p.peakTokensPerS > merged.peakTokensPerS {
			merged.peakTokensPerS = p.peakTokensPerS
		}
		// A merged total is priced only if at least one part was, and sums
		// only the priced parts — mixing an unpriced local deployment into a
		// cloud model's cost would understate it silently.
		if p.priced {
			merged.priced = true
			merged.costUSD += p.costUSD
		}
		for n := range p.nodes {
			merged.nodes[n] = struct{}{}
		}
		if merged.firstSeen.IsZero() || (!p.firstSeen.IsZero() && p.firstSeen.Before(merged.firstSeen)) {
			merged.firstSeen = p.firstSeen
		}
		if p.lastSeen.After(merged.lastSeen) {
			merged.lastSeen = p.lastSeen
		}
		if p.provider != "" {
			providers = append(providers, p.provider)
		}
	}

	out := merged.snapshot()
	sort.Strings(providers)
	out.Provider = strings.Join(providers, ",")
	return out
}
