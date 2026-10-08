package server

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A route that streams without saying so is a trap: a client reads the
// body to completion and blocks until its own timeout. Ring 1's contract
// test does exactly that, and hung on these two for as long as the
// catalog called them ordinary.
func TestStreamingForPathClassifiesSSERoutes(t *testing.T) {
	sse := []string{
		"/zzrouter/v1/model-groups/events",
		"/zzrouter/v1/spend/events",
		"/zzrouter/v1/jobs/:id/stream",
		"/zzrouter/v1/inference-logs/stream",
		"/zzrouter/v1/runs/:id/logs",
	}
	for _, p := range sse {
		assert.Equal(t, "sse", streamingForPath(http.MethodGet, p), "%s streams", p)
	}

	// OpenAI's fine-tuning events is a paginated list. It shares the
	// /events suffix with the two above, which is why they are named
	// explicitly instead of matched on the suffix.
	assert.Empty(t, streamingForPath(http.MethodGet, "/v1/fine_tuning/jobs/:job_id/events"))
	assert.Empty(t, streamingForPath(http.MethodGet, "/zzrouter/v1/providers"))
}
