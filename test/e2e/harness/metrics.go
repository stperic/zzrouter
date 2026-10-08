package harness

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// MetricSample is one decoded sample from a Prometheus exposition body:
// the metric name, its label set, and the numeric value. Used by tests
// asserting OTel emission contracts (gen_ai.* series presence + label
// correctness) without pulling in expfmt.
type MetricSample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// MetricSet is a queryable view over a single /metrics scrape. Built by
// ScrapeMetrics. The lookups (Sum, HistogramCount, etc.) match by
// metric name + the *required* labels — extra labels in the sample are
// ignored, so tests don't have to spell out every otel_scope_*
// attribute Prometheus tacks on automatically.
type MetricSet struct {
	Samples []MetricSample
}

// ScrapeMetrics fetches GET /metrics through the supplied admin client
// and parses the Prometheus text-exposition body. Errors only on
// transport / non-200 / malformed-line failures; an empty body decodes
// to an empty set.
func ScrapeMetrics(ctx context.Context, c *Client) (*MetricSet, error) {
	resp, err := c.GET(ctx, "/metrics")
	if err != nil {
		return nil, fmt.Errorf("ScrapeMetrics: GET /metrics: %w", err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("ScrapeMetrics: status=%d body=%q",
			resp.Status, truncateBytes(resp.Body, 200))
	}
	return ParseMetrics(string(resp.Body))
}

// ParseMetrics decodes a Prometheus text-exposition body. Exported so
// fixture tests can drive it without a live cluster.
func ParseMetrics(body string) (*MetricSet, error) {
	out := &MetricSet{}
	for i, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s, err := parseSampleLine(line)
		if err != nil {
			return nil, fmt.Errorf("ParseMetrics: line %d: %w", i+1, err)
		}
		out.Samples = append(out.Samples, s)
	}
	return out, nil
}

// parseSampleLine handles `metric_name{k="v",k2="v2"} 12.34` AND the
// labelless `metric_name 12.34` form. Trailing timestamps (a 3rd field
// in some exporters) are ignored. We don't fully escape-decode label
// values because OTel attribute keys + the values we care about are
// 7-bit ASCII without quotes/backslashes; the parser would just need a
// proper unescape pass to handle the general case.
func parseSampleLine(line string) (MetricSample, error) {
	openBrace := strings.IndexByte(line, '{')
	var name, labelsRaw, valueRaw string
	switch {
	case openBrace >= 0:
		closeBrace := strings.LastIndexByte(line, '}')
		if closeBrace < openBrace {
			return MetricSample{}, fmt.Errorf("unbalanced braces: %q", line)
		}
		name = line[:openBrace]
		labelsRaw = line[openBrace+1 : closeBrace]
		valueRaw = strings.TrimSpace(line[closeBrace+1:])
	default:
		// labelless: split on first space
		sp := strings.IndexByte(line, ' ')
		if sp <= 0 {
			return MetricSample{}, fmt.Errorf("no value separator: %q", line)
		}
		name = line[:sp]
		valueRaw = strings.TrimSpace(line[sp+1:])
	}
	// Drop any optional trailing timestamp.
	if sp := strings.IndexByte(valueRaw, ' '); sp > 0 {
		valueRaw = valueRaw[:sp]
	}
	v, err := strconv.ParseFloat(valueRaw, 64)
	if err != nil {
		return MetricSample{}, fmt.Errorf("value %q: %w", valueRaw, err)
	}
	return MetricSample{
		Name:   name,
		Labels: parseLabels(labelsRaw),
		Value:  v,
	}, nil
}

// parseLabels splits `k="v",k2="v2"` into a map. Quotes are stripped;
// commas inside values are not handled (OTel label values don't carry
// them in our emit surface).
func parseLabels(raw string) map[string]string {
	if raw == "" {
		return nil
	}
	out := map[string]string{}
	for _, kv := range splitLabels(raw) {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(kv[:eq])
		val := strings.Trim(strings.TrimSpace(kv[eq+1:]), `"`)
		out[key] = val
	}
	return out
}

// splitLabels splits a label-list at commas that aren't inside a quoted
// value. Stays naive about backslash-escaped quotes — fine for our
// emission surface.
func splitLabels(raw string) []string {
	var parts []string
	var inQuotes bool
	start := 0
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '"':
			inQuotes = !inQuotes
		case ',':
			if !inQuotes {
				parts = append(parts, raw[start:i])
				start = i + 1
			}
		}
	}
	if start < len(raw) {
		parts = append(parts, raw[start:])
	}
	return parts
}

// Match returns the first sample whose name equals exactly and whose
// labels are a superset of `required`. The boolean is false when no
// sample matches.
func (m *MetricSet) Match(name string, required map[string]string) (MetricSample, bool) {
	for _, s := range m.Samples {
		if s.Name != name {
			continue
		}
		if labelsMatch(s.Labels, required) {
			return s, true
		}
	}
	return MetricSample{}, false
}

// HistogramCount returns the value of <name>_count for the given
// required labels — Prometheus emits the histogram count as a separate
// series with the suffix "_count". The boolean is false when no series
// matches.
func (m *MetricSet) HistogramCount(name string, required map[string]string) (float64, bool) {
	s, ok := m.Match(name+"_count", required)
	if !ok {
		return 0, false
	}
	return s.Value, true
}

// HistogramSum mirrors HistogramCount but reads the _sum series.
func (m *MetricSet) HistogramSum(name string, required map[string]string) (float64, bool) {
	s, ok := m.Match(name+"_sum", required)
	if !ok {
		return 0, false
	}
	return s.Value, true
}

// AnyMatch reports whether any sample under `name` carries every label
// in `required`. Useful for "did this label key appear at all" checks
// where the count is irrelevant.
func (m *MetricSet) AnyMatch(name string, required map[string]string) bool {
	_, ok := m.Match(name, required)
	return ok
}

func labelsMatch(actual, required map[string]string) bool {
	for k, v := range required {
		if actual[k] != v {
			return false
		}
	}
	return true
}

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}
