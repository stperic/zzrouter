package config

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/inferencelog"
	"github.com/stretchr/testify/assert"
)

func TestInferenceLogConfig_GetMaxPayloads(t *testing.T) {
	off := 0
	custom := 25
	negative := -5
	disabled := false

	tests := []struct {
		name string
		cfg  InferenceLogConfig
		env  string
		want int
	}{
		{name: "absent uses the ring default", want: inferencelog.DefaultMaxPayloads},
		{name: "explicit zero disables", cfg: InferenceLogConfig{MaxPayloads: &off}, want: 0},
		{name: "explicit value wins", cfg: InferenceLogConfig{MaxPayloads: &custom}, want: 25},
		{name: "negative disables", cfg: InferenceLogConfig{MaxPayloads: &negative}, want: 0},
		{
			name: "prompt capture off suppresses payloads",
			cfg:  InferenceLogConfig{CapturePrompts: &disabled, MaxPayloads: &custom},
			want: 0,
		},
		{name: "env overrides config", cfg: InferenceLogConfig{MaxPayloads: &custom}, env: "7", want: 7},
		{name: "env disables", env: "0", want: 0},
		{name: "invalid env falls back to config", cfg: InferenceLogConfig{MaxPayloads: &custom}, env: "many", want: 25},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env != "" {
				t.Setenv("ZZROUTER_INFERENCE_LOG_PAYLOADS", tt.env)
			}
			assert.Equal(t, tt.want, tt.cfg.GetMaxPayloads())
		})
	}
}
