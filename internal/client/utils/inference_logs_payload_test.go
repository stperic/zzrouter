package client

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetInferenceLogPayload(t *testing.T) {
	var gotPath string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{
			"request":{"model":"fast-chat","messages":[{"role":"user","content":"hi"}]},
			"upstream_request":{"model":"llama3:8b","messages":[{"role":"user","content":"hi"}]},
			"elided":[{"path":"messages[0].images[0]","media":"image/png","bytes":204800}]}}`))
	}))

	payload, err := c.GetInferenceLogPayload("abc 123")
	require.NoError(t, err)

	assert.Equal(t, "/zzrouter/v1/inference-logs/abc%20123/payload", gotPath,
		"the ID must be path-escaped")
	assert.JSONEq(t, `{"model":"fast-chat","messages":[{"role":"user","content":"hi"}]}`,
		string(payload.Request))
	assert.JSONEq(t, `{"model":"llama3:8b","messages":[{"role":"user","content":"hi"}]}`,
		string(payload.Upstream))
	require.Len(t, payload.Elided, 1)
	assert.Equal(t, "image/png", payload.Elided[0].Media)
	assert.Equal(t, 204800, payload.Elided[0].Bytes)
}

func TestGetInferenceLogPayload_AgedOut(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"title":"Not Found","detail":"payload no longer retained for this entry"}`))
	}))

	_, err := c.GetInferenceLogPayload("gone")
	require.Error(t, err)
}

func TestInferenceLogEntry_DecodesPayloadAvailable(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":[
			{"id":"a","model":"m","payload_available":true},
			{"id":"b","model":"m"}]}`))
	}))

	entries, err := c.GetInferenceLogs("", "", "", 10)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.True(t, entries[0].PayloadAvailable)
	assert.False(t, entries[1].PayloadAvailable)
}
