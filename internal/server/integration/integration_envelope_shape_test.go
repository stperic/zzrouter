package integration_test

import (
	"encoding/json"
	"net/http"
	"testing"

	srv "github.com/stperic/zzrouter/internal/server"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Envelope shape contract for /zzrouter/v1/* admin handlers. Pinned
// after Arc F (commit 2ccfade1) cleaned up four flat-shape stragglers,
// to prevent a fifth from drifting in.
//
// Singular: {success: bool, message: string, data: any}
// List:     {data: [...], total: number, has_more?: bool, metadata?: any}
//
// Out of scope: SSE/log streams, raw blob pass-throughs, /v1/*, /api/*,
// /metrics, /openapi.{yaml,json}, /health/*. The matrix below is the
// curated surface — when a new admin GET ships, add a row.
const (
	shapeSingular = "singular"
	shapeList     = "list"
)

func TestIntegration_EnvelopeShape_Conformance(t *testing.T) {
	server := srv.NewTestNodeWithDefaults(t)

	tests := []struct {
		name  string
		path  string
		shape string
	}{
		{"api_discovery_root", "/zzrouter/v1", shapeSingular},
		{"system_info", "/zzrouter/v1/system", shapeSingular},
		{"server_identity", "/zzrouter/v1/server/identity", shapeSingular},
		{"server_logs_list", "/zzrouter/v1/server/logs", shapeList},
		{"nodes_list", "/zzrouter/v1/nodes", shapeList},
		// runs/capabilities and runs/ports return singular envelopes with
		// a structured payload under `data` (which may contain nested
		// lists). The list envelope is for endpoints whose answer IS a
		// collection, with top-level total/has_more.
		// /nodes/compatible requires a model query param — covered
		// indirectly by other tests.
		{"models_list", "/zzrouter/v1/models", shapeList},
		{"models_stats", "/zzrouter/v1/models/stats", shapeSingular},
		{"runs_list", "/zzrouter/v1/runs", shapeList},
		{"runs_capabilities", "/zzrouter/v1/runs/capabilities", shapeSingular},
		{"runs_metrics", "/zzrouter/v1/runs/metrics", shapeSingular},
		{"runs_ports", "/zzrouter/v1/runs/ports", shapeSingular},
		{"deployments_list", "/zzrouter/v1/deployments", shapeList},
		{"providers_list", "/zzrouter/v1/providers", shapeList},
		{"providers_catalog", "/zzrouter/v1/providers/catalog", shapeList},
		{"providers_status_list", "/zzrouter/v1/providers/status", shapeList},
		{"keys_list", "/zzrouter/v1/keys", shapeList},
		{"keys_schema", "/zzrouter/v1/keys/schema", shapeSingular},
		{"teams_list", "/zzrouter/v1/teams", shapeList},
		{"teams_schema", "/zzrouter/v1/teams/schema", shapeSingular},
		// jobs answers in the list envelope, with the owning node under
		// metadata.node. It used to nest the array at data.jobs behind
		// the singular envelope, which made it the one list endpoint on
		// this surface a caller had to special-case.
		{"jobs_list", "/zzrouter/v1/jobs", shapeList},
		{"inference_logs_list", "/zzrouter/v1/inference-logs", shapeList},
		{"pricing_status", "/zzrouter/v1/pricing/status", shapeSingular},
		{"update_status", "/zzrouter/v1/update/status", shapeSingular},
		{"update_history", "/zzrouter/v1/update/history", shapeList},
		{"spend_report", "/zzrouter/v1/spend/report", shapeSingular},
		{"cache_stats", "/zzrouter/v1/cache/stats", shapeSingular},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := srv.MakeAuthRequest(t, server, "GET", tt.path, srv.TestAdminKey, nil)

			// 503 is acceptable on a fresh test node when an optional
			// store (pricing, update, cluster) is uninitialized — the
			// shape contract only binds 2xx responses.
			if resp.Code == http.StatusServiceUnavailable {
				t.Logf("skip %s: 503 (uninitialized store on fresh node)", tt.path)
				return
			}
			require.Equalf(t, http.StatusOK, resp.Code,
				"expected 200, got %d; body=%s", resp.Code, string(resp.Body))

			assertEnvelopeShape(t, resp.Body, tt.shape)
		})
	}
}

// assertEnvelopeShape decodes body and verifies it matches the canonical
// singular or list envelope. Detection is by structural fingerprint, not
// by a single key sniff — `total` and `success` could each appear as
// rogue fields in a flat handler, so we check the full key set.
func assertEnvelopeShape(t *testing.T, body []byte, want string) {
	t.Helper()

	var env map[string]json.RawMessage
	require.NoErrorf(t, json.Unmarshal(body, &env),
		"body is not a JSON object: %s", string(body))

	switch want {
	case shapeSingular:
		// Required: success, message. data may be present or absent.
		assert.Containsf(t, env, "success", "singular envelope must include 'success'")
		assert.Containsf(t, env, "message", "singular envelope must include 'message'")
		// Catch list-shape leakage:
		assert.NotContainsf(t, env, "total",
			"singular envelope must not include 'total' (list shape leakage)")
		assert.NotContainsf(t, env, "has_more",
			"singular envelope must not include 'has_more' (list shape leakage)")

		var success bool
		require.NoError(t, json.Unmarshal(env["success"], &success))
		assert.Truef(t, success, "singular envelope success=true on 2xx")

	case shapeList:
		// Required: data (array), total (number). has_more / metadata optional.
		require.Containsf(t, env, "data", "list envelope must include 'data'")
		require.Containsf(t, env, "total", "list envelope must include 'total'")

		var data []json.RawMessage
		assert.NoErrorf(t, json.Unmarshal(env["data"], &data),
			"list envelope 'data' must be a JSON array")
		var total float64
		assert.NoErrorf(t, json.Unmarshal(env["total"], &total),
			"list envelope 'total' must be a JSON number")

		// Catch singular-shape leakage:
		assert.NotContainsf(t, env, "success",
			"list envelope must not include 'success' (singular shape leakage)")
		assert.NotContainsf(t, env, "message",
			"list envelope must not include 'message' (singular shape leakage)")

	default:
		t.Fatalf("unknown shape constant %q", want)
	}
}
