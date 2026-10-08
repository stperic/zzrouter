package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestExtractResponseIDFromSSE_ResponseCreated verifies the happy path:
// an OpenAI-shaped response.created frame at the head of the stream
// yields response.id.
func TestExtractResponseIDFromSSE_ResponseCreated(t *testing.T) {
	buf := []byte(`event: response.created
data: {"type":"response.created","response":{"id":"resp_abc123","object":"response"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"hi"}

`)
	assert.Equal(t, "resp_abc123", extractResponseIDFromSSE(buf))
}

// TestExtractResponseIDFromSSE_TopLevelIDFallback verifies the fallback
// path for backends that emit the response id at the top level of the
// first event payload instead of nested under "response".
func TestExtractResponseIDFromSSE_TopLevelIDFallback(t *testing.T) {
	buf := []byte(`data: {"id":"resp_xyz","object":"response"}

`)
	assert.Equal(t, "resp_xyz", extractResponseIDFromSSE(buf))
}

// TestExtractResponseIDFromSSE_IgnoresDoneSentinel verifies the [DONE]
// frame is skipped rather than tripping the JSON decoder.
func TestExtractResponseIDFromSSE_IgnoresDoneSentinel(t *testing.T) {
	buf := []byte(`data: [DONE]

data: {"id":"resp_after_done"}

`)
	assert.Equal(t, "resp_after_done", extractResponseIDFromSSE(buf))
}

// TestExtractResponseIDFromSSE_NoIDReturnsEmpty verifies that a buffer
// without any id yields empty string so the affinity recorder skips
// the Record call.
func TestExtractResponseIDFromSSE_NoIDReturnsEmpty(t *testing.T) {
	buf := []byte(`data: {"type":"response.output_text.delta","delta":"hi"}

`)
	assert.Equal(t, "", extractResponseIDFromSSE(buf))
}

// TestExtractResponseIDFromSSE_MalformedFramesAreSkipped verifies that
// non-JSON and non-data lines do not abort the scan.
func TestExtractResponseIDFromSSE_MalformedFramesAreSkipped(t *testing.T) {
	buf := []byte(`: keepalive comment
event: response.created
data: not-json

data: {"type":"response.created","response":{"id":"resp_ok"}}

`)
	assert.Equal(t, "resp_ok", extractResponseIDFromSSE(buf))
}
