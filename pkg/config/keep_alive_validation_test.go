package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderKeepAliveValidation(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    time.Duration
		invalid bool
	}{
		{"-1", -1, false}, {"0", 0, false}, {"", 5 * time.Minute, false}, {"30s", 30 * time.Second, false}, {"invalid", 0, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			duration, err := (&AppRuntimeConfig{KeepAlive: tc.value}).GetKeepAliveDuration()
			assert.Equal(t, tc.invalid, err != nil)
			assert.Equal(t, tc.want, duration)
			external := NewOllamaConnectProvider("test", "http://daemon:11434", "")
			external.Runtime.KeepAlive = tc.value
			onDemand := &OnDemandProvider{Name: "test", Protocol: ProtocolOpenAI, Enabled: new(true), Capabilities: external.Capabilities, Runtime: OnDemandRuntime{KeepAlive: tc.value, BasePort: 8200, Execution: ExecutionConfig{Type: "cli"}}}
			for _, provider := range []Provider{external, onDemand} {
				err := provider.Validate()
				if tc.invalid {
					require.ErrorContains(t, err, "keep_alive")
				} else {
					require.NoError(t, err)
				}
			}
		})
	}
}
