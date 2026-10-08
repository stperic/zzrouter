package install

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `"timeout": 10000000000` reads as ten billion seconds unless the reader
// already knows time.Duration marshals nanoseconds. The string carries its
// own unit.
func TestStepTimeoutMarshalsAsDurationString(t *testing.T) {
	data, err := json.Marshal(Step{Number: 1, Description: "d", Timeout: 30 * time.Second})
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	assert.Equal(t, "30s", raw["timeout"])
	assert.Equal(t, float64(1), raw["step"], "the other fields still marshal normally")

	// A step with no timeout omits the field rather than claiming "0s".
	// Decoded into a fresh map: json.Unmarshal merges into an existing one,
	// so a reused map would still hold the key from above.
	data, err = json.Marshal(Step{Number: 2})
	require.NoError(t, err)
	bare := map[string]any{}
	require.NoError(t, json.Unmarshal(data, &bare))
	assert.NotContains(t, bare, "timeout")
}

// The coordinator decodes plans a worker produced, so the two halves have
// to agree.
func TestStepTimeoutRoundTrips(t *testing.T) {
	original := Step{
		Number:      3,
		Description: "Download archive",
		Command:     "curl -o x y",
		Timeout:     30 * time.Minute,
		Verify:      StepVerify{Type: "file_exists", Path: "/tmp/x", Transient: true},
	}

	data, err := json.Marshal(original)
	require.NoError(t, err)

	var got Step
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, original, got)
}

func TestStepTimeoutUnmarshalForms(t *testing.T) {
	tests := []struct {
		name, body string
		want       time.Duration
		wantErr    bool
	}{
		{name: "duration string", body: `{"step":1,"timeout":"5m"}`, want: 5 * time.Minute},
		// A bare number is accepted so anything hand-building a step body
		// still decodes.
		{name: "nanosecond count", body: `{"step":1,"timeout":10000000000}`, want: 10 * time.Second},
		{name: "absent", body: `{"step":1}`, want: 0},
		{name: "null", body: `{"step":1,"timeout":null}`, want: 0},
		{name: "nonsense string", body: `{"step":1,"timeout":"soon"}`, wantErr: true},
		{name: "wrong type", body: `{"step":1,"timeout":true}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got Step
			err := json.Unmarshal([]byte(tt.body), &got)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.Timeout)
		})
	}
}

// A whole plan is what actually crosses the wire.
func TestPlanRoundTripsTimeouts(t *testing.T) {
	plan := Plan{
		Provider: "llamacpp",
		Version:  "b10516",
		Steps: []Step{
			{Number: 1, Timeout: 10 * time.Second},
			{Number: 2, Timeout: 30 * time.Minute},
			{Number: 3},
		},
	}

	data, err := json.Marshal(plan)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"timeout":"30m0s"`)

	var got Plan
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, plan.Steps, got.Steps)
}
