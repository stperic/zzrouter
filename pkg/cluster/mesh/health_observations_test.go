package mesh

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/host/inventory"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"

	"github.com/stretchr/testify/require"
)

func TestHealthObservationAbsenceAndUntrustedFallback(t *testing.T) {
	for _, scenario := range []string{"legacy", "invalid", "public"} {
		t.Run(scenario, func(t *testing.T) {
			data := map[string]any{"memory_total": map[string]any{"value": 1024}, "memory_used": map[string]any{"value": 0}, "activity": map[string]any{"state": "active", "active_requests": 1, "age_seconds": 0, "valid_for_seconds": 20}}
			if scenario != "legacy" {
				data["memory_sample"] = map[string]any{"total_bytes": 1024, "used_bytes": 0, "age_seconds": -1}
			}
			if scenario == "invalid" {
				data["activity"].(map[string]any)["age_seconds"] = -1
			}
			peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { require.NoError(t, json.NewEncoder(w).Encode(data)) }))
			defer peer.Close()
			u, err := url.Parse(peer.URL)
			require.NoError(t, err)
			port, err := strconv.Atoi(u.Port())
			require.NoError(t, err)
			if scenario == "public" {
				port = 0
			}
			connector := &Connector{httpClient: peer.Client(), clusterPort: port}
			result, err := connector.performHealthCheck(t.Context(), peer.URL)
			require.NoError(t, err)
			require.Nil(t, result.MemorySample)
			if scenario != "legacy" {
				require.Nil(t, result.Activity)
			}
		})
	}
}

func TestDegradedHealthRetainsAuthenticatedObservationsWithoutRenewal(t *testing.T) {
	registry := NewEndpointRegistry()
	url := "http://worker:9090"
	require.NoError(t, registry.RegisterEndpoint(&Endpoint{URL: url}))
	used := int64(17)
	old := inventory.NewMemorySample(64, &used, time.Now().Add(-10*time.Second))
	report := HealthReport{RAMTotalGB: 64, MemorySample: old, Activity: &instance.Activity{State: "active", ActiveRequests: 1, ObservedAt: time.Now(), ValidForSeconds: 30}}
	registry.Observe(url, ProbeResult{OK: true, Quality: QualityFull, Snapshot: &Connection{NodeName: "worker", HealthReport: report}})
	before, err := registry.GetEndpointByURL(url)
	require.NoError(t, err)
	registry.Observe(url, ProbeResult{OK: true, Quality: QualityDegraded, Snapshot: &Connection{Quality: QualityFull, NodeName: "untrusted", HealthReport: HealthReport{RAMTotalGB: 999}}})
	after, err := registry.GetEndpointByURL(url)
	require.NoError(t, err)
	require.Equal(t, StatusDegraded, after.Status)
	require.Equal(t, "worker", after.NodeName)
	require.Equal(t, before.Snapshot.CollectedAt, after.Snapshot.CollectedAt)
	require.Equal(t, old, after.Snapshot.MemorySample)
	require.Equal(t, float64(64), after.Snapshot.RAMTotalGB)
	require.True(t, after.Snapshot.MemorySample.Current(time.Now(), after.Status != StatusUp).Stale)
	require.Equal(t, "unknown", after.Snapshot.Activity.Current(time.Now(), after.Status != StatusUp).State)
}

func TestRegistryObservationsAreIndependentOnInputAndRead(t *testing.T) {
	at := time.Now().UTC()
	makeReport := func() HealthReport {
		used, index, available := int64(17), 1, float64(47)
		return HealthReport{
			MemorySample:     &inventory.MemorySample{TotalBytes: 64, UsedBytes: &used, ObservedAt: at},
			Activity:         &instance.Activity{State: "active", ActiveRequests: 1, ObservedAt: at, ValidForSeconds: 30},
			ProviderActivity: map[string]*instance.Activity{"engine": {State: "active", ActiveRequests: 1, ObservedAt: at, ValidForSeconds: 30}},
			GPUs:             []GPUDetail{{Sample: &inventory.MemorySample{TotalBytes: 64, UsedBytes: &used, ObservedAt: at}, DriverIndex: &index, MemoryAvailGB: &available}},
		}
	}
	mutate := func(report *HealthReport) {
		*report.MemorySample.UsedBytes = 999
		report.Activity.State = "corrupted"
		report.ProviderActivity["engine"].State = "corrupted"
		report.ProviderActivity["injected"] = nil
		report.GPUs[0].Sample.Stale = true
		*report.GPUs[0].Sample.UsedBytes = 999
		*report.GPUs[0].DriverIndex = 999
		*report.GPUs[0].MemoryAvailGB = 999
	}
	for _, source := range []string{"remote", "local", "register"} {
		t.Run(source, func(t *testing.T) {
			registry := NewEndpointRegistry()
			url := "http://worker:9090"
			report := makeReport()
			read := func() *Endpoint { value, err := registry.GetEndpointByURL(url); require.NoError(t, err); return value }
			switch source {
			case "remote":
				require.NoError(t, registry.RegisterEndpoint(&Endpoint{URL: url}))
				registry.Observe(url, ProbeResult{OK: true, Quality: QualityFull, Snapshot: &Connection{HealthReport: report}})
			case "local":
				registry.ObserveLocal(EndpointSnapshot{HealthReport: report})
				read = registry.Self
			case "register":
				require.NoError(t, registry.RegisterEndpoint(&Endpoint{URL: url, Snapshot: EndpointSnapshot{HealthReport: report}}))
			}
			mutate(&report)
			require.Equal(t, makeReport(), read().Snapshot.HealthReport, "retained inputs must be independent")
			returned := read()
			mutate(&returned.Snapshot.HealthReport)
			require.Equal(t, makeReport(), read().Snapshot.HealthReport, "returned observations must be independent")
		})
	}
}
