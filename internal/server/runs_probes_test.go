package server

// Behavior test for GET /zzrouter/v1/runs/:id/probes (moved from
// /instances/:id/probes in API-shape arc 1.3). Local path only: the
// ?node= dispatch goes through the cluster unicast machinery, which
// needs a multi-node harness and is covered by the e2e suites.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunProbes_LocalInstance(t *testing.T) {
	server := createTestNodeWithDefaults(t)
	id := newLogsTestInstance(t, server, "it-probes-1", []string{"startup ok"})

	resp := makeAuthRequest(t, server, http.MethodGet, "/zzrouter/v1/runs/"+id+"/probes", TestAdminKey, nil)
	require.Equal(t, http.StatusOK, resp.Code, string(resp.Body))

	var envelope struct {
		Data struct {
			InstanceID string `json:"instance_id"`
			Status     string `json:"status"`
			LogFile    string `json:"log_file"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &envelope))
	require.Equal(t, id, envelope.Data.InstanceID)
	require.NotEmpty(t, envelope.Data.Status)
	require.NotEmpty(t, envelope.Data.LogFile)
}

func TestRunProbes_UnknownInstance(t *testing.T) {
	server := createTestNodeWithDefaults(t)

	resp := makeAuthRequest(t, server, http.MethodGet, "/zzrouter/v1/runs/no-such-run/probes", TestAdminKey, nil)
	require.Equal(t, http.StatusNotFound, resp.Code, string(resp.Body))
}
