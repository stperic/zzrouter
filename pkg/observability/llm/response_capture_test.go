package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withCapture flips reply capture on for one test and restores it, since
// the gate is process-wide.
func withCapture(t *testing.T, enabled bool) {
	t.Helper()
	prev := CaptureResponses()
	SetCaptureResponses(enabled)
	t.Cleanup(func() { SetCaptureResponses(prev) })
}

func TestAppendResponseText_GatedOff(t *testing.T) {
	withCapture(t, false)
	r := NewInferenceRecorder(context.Background(), "m", "p")

	r.AppendResponseText("should not be kept")
	r.SetResponseBody([]byte(`{"a":1}`))

	assert.Empty(t, r.responseText.String())
	assert.Empty(t, r.responseBody)
}

func TestAppendResponseText_AccumulatesInOrder(t *testing.T) {
	withCapture(t, true)
	r := NewInferenceRecorder(context.Background(), "m", "p")

	for _, part := range []string{"Hey ", "Eric", "!"} {
		r.AppendResponseText(part)
	}

	assert.Equal(t, "Hey Eric!", r.responseText.String())
}

// One runaway generation must not pin memory in the log ring.
func TestAppendResponseText_StopsAtTheCap(t *testing.T) {
	withCapture(t, true)
	r := NewInferenceRecorder(context.Background(), "m", "p")

	for i := 0; i < 10; i++ {
		r.AppendResponseText(strings.Repeat("x", maxResponseTextBytes/4))
	}

	assert.Equal(t, maxResponseTextBytes, r.responseText.Len())
}

func TestAppendResponseText_NilRecorderAndEmptyInput(t *testing.T) {
	withCapture(t, true)
	var nilRecorder *InferenceRecorder
	require.NotPanics(t, func() {
		nilRecorder.AppendResponseText("x")
		nilRecorder.SetResponseBody([]byte("x"))
	})

	r := NewInferenceRecorder(context.Background(), "m", "p")
	r.AppendResponseText("")
	assert.Empty(t, r.responseText.String())
}
