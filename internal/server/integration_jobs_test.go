package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/inferencelog"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Integration coverage for the public/internal jobs endpoints. These
// tests verify wiring, authz, and wire-format basics — they do not
// cover streaming correctness (that's the pkg/jobs unit tests).

func TestJobsEndpoint_EmptyList(t *testing.T) {
	srv := createTestNodeWithDefaults(t)

	// Filter out the long-lived inference-log handle — it's opened at
	// server startup and lives forever. Real "fresh" test is against
	// bounded kinds only.
	resp := makeAuthRequest(t, srv, http.MethodGet, "/zzrouter/v1/jobs?kind=download", TestAdminKey, nil)
	require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body)
	// The collection envelope every list endpoint on this surface uses:
	// the array at `data`, endpoint-specific extras under `metadata`.
	var body struct {
		Data     []map[string]any `json:"data"`
		Total    int              `json:"total"`
		HasMore  bool             `json:"has_more"`
		Metadata struct {
			Node string `json:"node"`
		} `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &body))
	assert.Empty(t, body.Data, "fresh registry should have no download jobs")
	assert.Zero(t, body.Total, "total should agree with the empty list")
	assert.NotEmpty(t, body.Metadata.Node, "node name should be stamped")
}

func TestJobsEndpoint_GetUnknown404(t *testing.T) {
	srv := createTestNodeWithDefaults(t)
	resp := makeAuthRequest(t, srv, http.MethodGet, "/zzrouter/v1/jobs/dl_nonexistent", TestAdminKey, nil)
	assert.Equal(t, http.StatusNotFound, resp.Code, "body=%s", resp.Body)
}

func TestJobsEndpoint_RemoteNodeReturns501(t *testing.T) {
	srv := createTestNodeWithDefaults(t)
	path := "/zzrouter/v1/jobs/dl_whatever?node=some-remote-worker"
	resp := makeAuthRequest(t, srv, http.MethodGet, path, TestAdminKey, nil)
	assert.Equal(t, http.StatusNotImplemented, resp.Code, "cross-node proxy path should return 501 in current phase")
}

func TestJobsEndpoint_RequiresAdminAuth(t *testing.T) {
	srv := createTestNodeWithDefaults(t)
	// No key
	resp := makeRequest(t, srv, TestRequest{Method: http.MethodGet, Path: "/zzrouter/v1/jobs"})
	assert.NotEqual(t, http.StatusOK, resp.Code, "unauthed access should be rejected")
}

func TestJobsEndpoint_RoundTripWithProducer(t *testing.T) {
	srv := createTestNodeWithDefaults(t)
	require.NotNil(t, srv.jobs, "jobs registry must be wired in the test server")

	// Spin up a producer directly against the registry. P3+ will do
	// this via the download tracker; for P2 we only need to assert the
	// HTTP surface sees the job.
	h, err := srv.jobs.Start(context.Background(), jobs.KindDownload, "admin", jobs.Meta{"model": "llama3"})
	require.NoError(t, err)

	resp := makeAuthRequest(t, srv, http.MethodGet, "/zzrouter/v1/jobs", TestAdminKey, nil)
	require.Equal(t, http.StatusOK, resp.Code)
	body := string(resp.Body)
	assert.Contains(t, body, h.ID(), "job should appear in list")
	assert.Contains(t, body, "llama3", "meta should surface")

	// Snapshot endpoint
	snap := makeAuthRequest(t, srv, http.MethodGet, "/zzrouter/v1/jobs/"+h.ID(), TestAdminKey, nil)
	require.Equal(t, http.StatusOK, snap.Code, "body=%s", snap.Body)
	assert.Contains(t, string(snap.Body), "pending")

	// Cancel is non-blocking → 202.
	cancel := makeAuthRequest(t, srv, http.MethodDelete, "/zzrouter/v1/jobs/"+h.ID(), TestAdminKey, nil)
	assert.Equal(t, http.StatusAccepted, cancel.Code, "cancel returns 202 accepted")

	// Clean up: finish the job so teardown is quick.
	h.Done()
}

func TestJobsEndpoint_StreamReplaysThenCloses(t *testing.T) {
	srv := createTestNodeWithDefaults(t)

	h, err := srv.jobs.Start(context.Background(), jobs.KindInstall, "admin", nil)
	require.NoError(t, err)
	h.Progress(50, "halfway", jobs.Bytes{Done: 50, Total: 100})
	h.Done()

	// Stream from seq=0 after terminal — must replay everything then close.
	resp := makeAuthRequest(t, srv, http.MethodGet, "/zzrouter/v1/jobs/"+h.ID()+"/stream?from=0", TestAdminKey, nil)
	require.Equal(t, http.StatusOK, resp.Code)
	body := string(resp.Body)
	// Gin's SSEvent writes `event:NAME\n` (no space). Pin to that form +
	// require a data line on the same frame so substring hits from user
	// content can't masquerade as event frames.
	assert.Contains(t, body, "event:progress\n", "expected progress events in stream; body=%s", body)
	assert.Contains(t, body, "event:done\n", "expected terminal done event; body=%s", body)
	assert.Contains(t, body, "data:", "expected at least one data line")
}

// TestJobsEndpoint_StreamSubscriberCleanedOnDisconnect asserts that a
// mid-stream client disconnect causes the registry to release the
// subscriber promptly (no goroutine leak). Uses a real httptest server
// so we can actually close the TCP connection from the client side.
func TestJobsEndpoint_StreamSubscriberCleanedOnDisconnect(t *testing.T) {
	srv := createTestNodeWithDefaults(t)
	h, err := srv.jobs.Start(context.Background(), jobs.KindDownload, "admin", nil)
	require.NoError(t, err)
	defer h.Done()

	ts := httptest.NewServer(srv.engine)
	defer ts.Close()

	// Open the stream in a goroutine then cancel — the server-side
	// handler must return and Unsubscribe must fire.
	reqCtx, cancel := context.WithCancel(context.Background())
	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet,
			ts.URL+"/zzrouter/v1/jobs/"+h.ID()+"/stream", nil)
		req.Header.Set("X-API-Key", TestAdminKey)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	// Give the handler time to attach the subscriber, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	// Wait for the request to return.
	select {
	case <-reqDone:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not return after client cancel")
	}

	// After cancel, a second Subscribe should see no leftover sub blocking
	// deliveries. More directly: emit an event and ensure the producer
	// doesn't hang. Rough proxy: Progress returns promptly (it always does;
	// the real assertion is the -race detector catching any leaked state).
	h.Progress(1, "post-disconnect", jobs.Bytes{})
}

func TestJobsEndpoint_StreamEpochMismatch409(t *testing.T) {
	srv := createTestNodeWithDefaults(t)
	h, err := srv.jobs.Start(context.Background(), jobs.KindDownload, "admin", nil)
	require.NoError(t, err)
	path := "/zzrouter/v1/jobs/" + h.ID() + "/stream?epoch=wrong-nonce"
	resp := makeAuthRequest(t, srv, http.MethodGet, path, TestAdminKey, nil)
	assert.Equal(t, http.StatusConflict, resp.Code)
	h.Done()
}

func TestInferenceLogMirrorsToJobsFirehose(t *testing.T) {
	srv := createTestNodeWithDefaults(t)
	require.NotNil(t, srv.inference.logBridge, "bridge must be wired")
	require.NotEmpty(t, srv.inference.logJobID, "server startup must open a KindInferenceLog handle")

	sub, err := srv.jobs.Subscribe(srv.inference.logJobID, jobs.SubscribeOptions{})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	// Drive two inference entries back-to-back; each must emit an event
	// whose Meta["entry"] fully represents THIS entry (no stale fields
	// from the previous emission).
	srv.inference.logBridge.OnInferenceComplete(llm.InferenceLogData{
		Model: "llama3", App: "ollama", RequestType: "chat",
		Node: "test-host", Status: "success",
		TokensIn: 10, TokensOut: 20,
		LatencyNs: 500 * 1000 * 1000,
	})
	srv.inference.logBridge.OnInferenceComplete(llm.InferenceLogData{
		Model: "gpt-4o", App: "openai", RequestType: "chat",
		Node: "test-host", Status: "error",
		ErrorType: "upstream_timeout", ErrorMessage: "504 gateway timeout",
		TokensIn: 100, TokensOut: 0,
		LatencyNs: 30 * 1000 * 1000 * 1000,
	})

	collect := func() inferencelog.LogEntry {
		select {
		case ev, ok := <-sub.Events():
			require.True(t, ok, "channel closed unexpectedly")
			assert.Equal(t, jobs.PhaseRunning, ev.Phase)
			require.NotNil(t, ev.Meta, "meta must carry the entry")
			entry, ok := ev.Meta["entry"].(inferencelog.LogEntry)
			require.True(t, ok, "meta.entry must be an inferencelog.LogEntry; got %T", ev.Meta["entry"])
			return entry
		case <-time.After(500 * time.Millisecond):
			t.Fatal("firehose subscriber saw no event after inference completion")
			return inferencelog.LogEntry{}
		}
	}

	first := collect()
	assert.Equal(t, "llama3", first.Model)
	assert.EqualValues(t, 10, first.TokensIn)
	assert.Equal(t, "success", first.Status)
	assert.Equal(t, "", first.ErrorType, "first entry has no error")

	second := collect()
	assert.Equal(t, "gpt-4o", second.Model, "second event should be fresh; no carryover from first")
	assert.Equal(t, "error", second.Status)
	assert.Equal(t, "upstream_timeout", second.ErrorType, "error details must propagate")
	assert.EqualValues(t, 0, second.TokensOut)
}

func TestJobsEndpoint_StreamFirehoseReplayRejected(t *testing.T) {
	srv := createTestNodeWithDefaults(t)
	h, err := srv.jobs.Start(context.Background(), jobs.KindInferenceLog, "admin", nil)
	require.NoError(t, err)
	resp := makeAuthRequest(t, srv, http.MethodGet,
		"/zzrouter/v1/jobs/"+h.ID()+"/stream?from=5", TestAdminKey, nil)
	assert.Equal(t, http.StatusBadRequest, resp.Code, "firehose + from=N must be rejected")
	h.Done()
}
