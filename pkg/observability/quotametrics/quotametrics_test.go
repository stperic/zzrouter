package quotametrics

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/sdk/metric"

	"github.com/stperic/zzrouter/pkg/access/quota"
)

// setupPromMeter builds a local OTel meter provider with a Prometheus
// exporter so tests can assert the exported series names and labels
// end-to-end (interface → OTel SDK → Prometheus text format).
func setupPromMeter(t *testing.T) *prometheus.Registry {
	t.Helper()

	reg := prometheus.NewRegistry()
	exporter, err := otelprom.New(otelprom.WithRegisterer(reg))
	require.NoError(t, err)

	provider := metric.NewMeterProvider(metric.WithReader(exporter))
	prior := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(prior)
		_ = provider.Shutdown(context.Background())
	})
	return reg
}

// sumFamily flattens every sample of a gathered metric family into one
// number. For counters and up/down-counters this is the total; for
// gauges it's the (multi-label) sum. Tests that care about per-label
// splits should inspect the *dto.MetricFamily directly.
func sumFamily(mfs []*dto.MetricFamily, name string) (float64, bool) {
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		var total float64
		for _, m := range mf.GetMetric() {
			switch {
			case m.Counter != nil:
				total += m.GetCounter().GetValue()
			case m.Gauge != nil:
				total += m.GetGauge().GetValue()
			}
		}
		return total, true
	}
	return 0, false
}

func TestRecorder_EndToEnd_ExportsExpectedFamilies(t *testing.T) {
	reg := setupPromMeter(t)

	rec, err := New()
	require.NoError(t, err)

	rec.ObserveDecision(quota.ScopeKey, "allowed", "")
	rec.ObserveDecision(quota.ScopeTeam, "denied", "rpm_limit_exceeded")
	rec.ObserveSpendSettlement(quota.ScopeKey, 0.42)
	rec.ObserveConcurrencyAcquire(quota.ScopeTeam)
	rec.ObserveConcurrencyAcquire(quota.ScopeTeam)
	rec.ObserveConcurrencyRelease(quota.ScopeTeam)

	mfs, err := reg.Gather()
	require.NoError(t, err)

	cases := []struct {
		family string
		want   float64
	}{
		{"zzrouter_quota_decision_total", 2},
		{"zzrouter_quota_spend_settled_usd_total", 0.42},
		{"zzrouter_quota_concurrency_active", 1}, // 2 acquires - 1 release
	}
	for _, tc := range cases {
		got, ok := sumFamily(mfs, tc.family)
		if !ok {
			names := make([]string, 0, len(mfs))
			for _, mf := range mfs {
				names = append(names, mf.GetName())
			}
			t.Fatalf("family %q not exported; have: %v", tc.family, names)
		}
		require.InDelta(t, tc.want, got, 0.0001, "family %q total", tc.family)
	}
}

// TestRecorder_LabelAttribution pins that the scope/outcome/reason
// labels land on the series they were emitted with — a silent
// attribution bug (e.g. always tagging `scope="key"`) would be
// invisible to the sum-only assertion in the end-to-end test.
func TestRecorder_LabelAttribution(t *testing.T) {
	reg := setupPromMeter(t)
	rec, err := New()
	require.NoError(t, err)

	rec.ObserveDecision(quota.ScopeTeam, "denied", "rpm_limit_exceeded")

	mfs, err := reg.Gather()
	require.NoError(t, err)

	var fam *dto.MetricFamily
	for _, mf := range mfs {
		if mf.GetName() == "zzrouter_quota_decision_total" {
			fam = mf
			break
		}
	}
	require.NotNil(t, fam, "decision counter not exported")

	var samples int
	for _, m := range fam.GetMetric() {
		labels := map[string]string{}
		for _, l := range m.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		if labels["scope"] != "team" {
			continue
		}
		if labels["outcome"] != "denied" {
			continue
		}
		if labels["reason"] != "rpm_limit_exceeded" {
			continue
		}
		samples++
		require.Equal(t, float64(1), m.GetCounter().GetValue(),
			"expected exactly one increment on the matching series")
	}
	require.Equal(t, 1, samples, "labeled series not found on decision counter")
}

func TestRecorder_ImplementsInterface(t *testing.T) {
	setupPromMeter(t)
	rec, err := New()
	require.NoError(t, err)
	var _ quota.MetricsRecorder = rec
}

func TestRecorder_NilSafe(t *testing.T) {
	var r *Recorder
	r.ObserveDecision(quota.ScopeKey, "allowed", "")
	r.ObserveSpendSettlement(quota.ScopeKey, 1.0)
	r.ObserveConcurrencyAcquire(quota.ScopeKey)
	r.ObserveConcurrencyRelease(quota.ScopeKey)
}

// TestMetricNames_Stable pins wire-format strings so a future rename
// refactor can't silently break dashboards and alert rules.
func TestMetricNames_Stable(t *testing.T) {
	cases := map[string]string{
		metricDecisionTotal:     "zzrouter.quota.decision.total",
		metricSpendSettledUSD:   "zzrouter.quota.spend.settled.usd",
		metricConcurrencyActive: "zzrouter.quota.concurrency.active",
		attrScope:               "scope",
		attrOutcome:             "outcome",
		attrReason:              "reason",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("wire-name drift: %q != %q", got, want)
		}
	}
}
