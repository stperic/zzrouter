package wire

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/stperic/zzrouter/pkg/observability/llm"
)

func TestStreamTransform(t *testing.T) {
	const usageObjectFrame = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3}}\n\n"
	const nullUsageFrame = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n"

	tests := []struct {
		name         string
		meta         RoutingMetadata
		chunk        string
		wantNilFunc  bool
		wantContains []string
		wantAbsent   []string
		wantUsage    bool
	}{
		{
			name:        "no stages leaves Func nil",
			meta:        RoutingMetadata{Provider: "openai"},
			chunk:       nullUsageFrame,
			wantNilFunc: true,
		},
		{
			name:         "client model rewrites the model on each frame",
			meta:         RoutingMetadata{Provider: "openai", ClientModel: "x"},
			chunk:        "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
			wantContains: []string{`"model":"x"`},
			wantAbsent:   []string{`"model":"m"`},
		},
		{
			name:       "suppress usage frame drops a usage-only frame",
			meta:       RoutingMetadata{Provider: "openai", SuppressUsageFrame: true},
			chunk:      usageObjectFrame,
			wantAbsent: []string{"data:", "prompt_tokens"},
		},
		{
			// A null usage on a delta frame is not a usage block: the synthetic-usage fallback must still fire.
			name:      "inject usage with null usage on a delta does not count as usage",
			meta:      RoutingMetadata{Provider: "openai", InjectUsage: true},
			chunk:     nullUsageFrame,
			wantUsage: false,
		},
		{
			name:      "inject usage with a usage object counts as usage",
			meta:      RoutingMetadata{Provider: "openai", InjectUsage: true},
			chunk:     usageObjectFrame,
			wantUsage: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := llm.NewInferenceRecorder(context.Background(), "m", "openai")
			st := NewStreamTransform(nil, tt.meta, rec)

			fn := st.Func()
			if tt.wantNilFunc {
				assert.Nil(t, fn)
				return
			}
			if assert.NotNil(t, fn) {
				out := string(fn([]byte(tt.chunk)))
				for _, s := range tt.wantContains {
					assert.Contains(t, out, s)
				}
				for _, s := range tt.wantAbsent {
					assert.NotContains(t, out, s)
				}
			}
			assert.Equal(t, tt.wantUsage, st.UsageSeen())
		})
	}
}
