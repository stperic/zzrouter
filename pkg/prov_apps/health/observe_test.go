package health

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/prov_apps/instance"

	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/require"
)

func observationRun(t *testing.T, runtime string) *instance.Instance {
	t.Helper()
	run := instance.NewInstance("observe", "mlx", "model", 8188, 0, 0)
	t.Cleanup(run.Cancel)
	run.SetResolved(instance.Resolved{Runtime: runtime})
	return run
}

func observationClient(status int, body string) *http.Client {
	return &http.Client{Transport: queueTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
}

func TestReadObservationCombinesPrometheusLabelSets(t *testing.T) {
	const body = `# TYPE vllm:running gauge
vllm:running{model="model-a",engine="0"} 3
vllm:running{model="model-a",engine="1"} 4
vllm:running{model="model-b",engine="0"} 5
# TYPE vllm:waiting gauge
vllm:waiting{engine="0"} 0
vllm:waiting{engine="1"} 2
# TYPE vllm:cache gauge
vllm:cache{engine="0"} 0.8
vllm:cache{engine="1"} 0.2
`
	observe := &schema.Observe{Path: "/metrics", Format: "prometheus", Running: "vllm:running", Waiting: "vllm:waiting", KVCacheUsage: "vllm:cache"}
	actual := ReadObservation(t.Context(), observationClient(http.StatusOK, body), observationRun(t, ""), observe)
	require.Equal(t, Observation{Running: new(12.0), Waiting: new(2.0), KVCacheUsage: new(0.8)}, actual)
}

func TestReadObservationMissingSeriesIsNull(t *testing.T) {
	observe := &schema.Observe{Path: "/metrics", Format: "prometheus", Running: "running", Waiting: "waiting"}
	actual := ReadObservation(t.Context(), observationClient(http.StatusOK, "waiting 0\n"), observationRun(t, ""), observe)
	require.Equal(t, Observation{Waiting: new(0.0), Cause: ObservationSeriesAbsent}, actual)
}

func TestReadObservationReportsErrorStatusAndMalformedText(t *testing.T) {
	observe := &schema.Observe{Path: "/metrics", Format: "prometheus", Running: "running"}
	actual := ReadObservation(t.Context(), observationClient(http.StatusNotImplemented, "metrics disabled"), observationRun(t, ""), observe)
	require.Equal(t, Observation{Cause: ObservationErrorStatus, StatusCode: http.StatusNotImplemented}, actual)
	actual = ReadObservation(t.Context(), observationClient(http.StatusOK, "running invalid\n"), observationRun(t, ""), observe)
	require.Equal(t, Observation{Cause: ObservationMalformed}, actual)
}

func TestReadObservationJSONDottedPaths(t *testing.T) {
	observe := &schema.Observe{Path: "/metrics", Format: "json", Running: "summary.in_flight", Waiting: "server.request_queue_depth", KVCacheUsage: "cache.usage"}
	actual := ReadObservation(t.Context(), observationClient(http.StatusOK, `{"summary":{"in_flight":3},"server":{"request_queue_depth":0},"cache":{"usage":0.25}}`), observationRun(t, ""), observe)
	require.Equal(t, Observation{Running: new(3.0), Waiting: new(0.0), KVCacheUsage: new(0.25)}, actual)
	actual = ReadObservation(t.Context(), observationClient(http.StatusOK, `{"summary":{},"server":{"request_queue_depth":"0"},"cache":{"usage":0.25}}`), observationRun(t, ""), observe)
	require.Equal(t, Observation{Cause: ObservationMalformed, KVCacheUsage: new(0.25)}, actual)
}

func TestReadObservationRequiresEveryBusyField(t *testing.T) {
	observe := queueConfig().Observe
	for body, cause := range map[string]ObservationCause{
		`{"queue_running":[1]}`:                      ObservationSeriesAbsent,
		`{"queue_running":[1],"queue_pending":null}`: ObservationMalformed,
	} {
		result := ReadObservation(t.Context(), observationClient(http.StatusOK, body), observationRun(t, ""), observe)
		require.Equal(t, Observation{Cause: cause}, result)
	}
}

func TestReadObservationEnforcesPayloadCap(t *testing.T) {
	const capBytes = 1 << 20
	for format, body := range map[string]string{"json": `{"running":1}`, "prometheus": "running 1\n# padding\n"} {
		t.Run(format, func(t *testing.T) {
			observe := &schema.Observe{Path: "/metrics", Format: format, Running: "running"}
			padding := strings.Repeat(" ", capBytes-len(body))
			if format == "prometheus" {
				body = strings.TrimSuffix(body, "\n") + padding + "\n"
			} else {
				body += padding
			}
			atLimit := ReadObservation(t.Context(), observationClient(http.StatusOK, body), observationRun(t, ""), observe)
			require.Equal(t, Observation{Running: new(1.0)}, atLimit)
			overLimit := ReadObservation(t.Context(), observationClient(http.StatusOK, body+" "), observationRun(t, ""), observe)
			require.Equal(t, Observation{Cause: ObservationMalformed}, overLimit)
		})
	}
}

func TestReadObservationDoesNotFollowRedirects(t *testing.T) {
	for _, format := range []string{"prometheus", "json"} {
		t.Run(format, func(t *testing.T) {
			client := &http.Client{Transport: queueTransport(func(req *http.Request) (*http.Response, error) {
				require.Equal(t, "localhost:8188", req.URL.Host)
				require.Equal(t, http.MethodGet, req.Method)
				return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"http://another.example/metrics"}}, Body: http.NoBody}, nil
			})}
			observe := &schema.Observe{Path: "/metrics", Format: format, Running: "running"}
			actual := ReadObservation(t.Context(), client, observationRun(t, ""), observe)
			require.Equal(t, Observation{Cause: ObservationErrorStatus, StatusCode: http.StatusFound}, actual)
		})
	}
}

func TestReadObservationReportsUnreachableEngine(t *testing.T) {
	client := &http.Client{Transport: queueTransport(func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})}
	observe := &schema.Observe{Path: "/metrics", Format: "json", Running: "running"}
	require.Equal(t, Observation{Cause: ObservationUnreachable}, ReadObservation(t.Context(), client, observationRun(t, ""), observe))
}

func TestReadObservationSelectsEffectiveRuntime(t *testing.T) {
	declared, err := schema.LoadYAMLSchema([]byte("observe: {path: /metrics, format: json, running: summary.in_flight, waiting: server.request_queue_depth, runtimes: [mlx-vlm]}"))
	require.NoError(t, err)
	calls := 0
	client := &http.Client{Transport: queueTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, "/metrics", req.URL.Path)
		body := `{"summary":{"in_flight":3},"server":{"request_queue_depth":2}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	for _, base := range []string{"mlx", ""} {
		require.Equal(t, Observation{}, ReadObservation(t.Context(), client, observationRun(t, base), declared.Observe))
	}
	require.Zero(t, calls)
	got := ReadObservation(t.Context(), client, observationRun(t, "mlx-vlm"), declared.Observe)
	require.Equal(t, Observation{Running: new(3.0), Waiting: new(2.0)}, got)
	require.Equal(t, 1, calls)

	declared.Observe.Runtimes = []string{"base"}
	require.Equal(t, Observation{}, ReadObservation(t.Context(), client, observationRun(t, "mlx-vlm"), declared.Observe))
	require.Equal(t, got, ReadObservation(t.Context(), client, observationRun(t, "mlx"), declared.Observe))
	require.Equal(t, 2, calls)
}
