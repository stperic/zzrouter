package proxy

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestInflight_NilSafety pins the no-op shape so init failures don't
// take down the request path. Both nil receiver and zero-value token
// must be safe.
func TestInflight_NilSafety(t *testing.T) {
	var m *InflightMetrics
	tok := m.IncInflight(context.Background(), "chat", "vllm", "llama2", "10.0.0.1")
	m.DecInflight(context.Background(), tok)

	// Package-level shortcuts on a nil-init meter must also no-op.
	tok2 := IncInflight(context.Background(), "chat", "vllm", "llama2", "")
	DecInflight(context.Background(), tok2)
}

// TestInflight_TokenCarriesAttrs pins that Inc captures the supplied
// labels onto the token so DecInflight can emit the matching -1.
// Mismatched labels would corrupt the saturation gauge by leaving
// ghost +1 / -1 series rather than netting to zero.
func TestInflight_TokenCarriesAttrs(t *testing.T) {
	m := GetInflightMetrics()
	if m == nil {
		t.Skip("meter init failed in test env")
	}
	tok := m.IncInflight(context.Background(), "chat", "vllm", "llama2:7b", "10.0.0.1")
	defer m.DecInflight(context.Background(), tok)

	keys := map[string]string{}
	for _, kv := range tok.attrs {
		keys[string(kv.Key)] = kv.Value.AsString()
	}
	assert.Equal(t, "chat", keys["gen_ai.operation.name"])
	assert.Equal(t, "vllm", keys["gen_ai.provider.name"])
	assert.Equal(t, "llama2:7b", keys["gen_ai.request.model"])
	assert.Equal(t, "10.0.0.1", keys["server.address"])
}

// TestInflight_OmitsEmptyOptionalLabels pins that empty requestModel /
// serverAddress are NOT emitted as labels (closed-enum semantics: an
// empty string is a distinct dimension value and would silently
// inflate Prometheus storage). Operation/provider always emit even
// when empty so the metric has at least one stable identifier.
func TestInflight_OmitsEmptyOptionalLabels(t *testing.T) {
	m := GetInflightMetrics()
	if m == nil {
		t.Skip("meter init failed in test env")
	}
	tok := m.IncInflight(context.Background(), "chat", "vllm", "", "")
	defer m.DecInflight(context.Background(), tok)

	keys := map[string]bool{}
	for _, kv := range tok.attrs {
		keys[string(kv.Key)] = true
	}
	assert.True(t, keys["gen_ai.operation.name"])
	assert.True(t, keys["gen_ai.provider.name"])
	assert.False(t, keys["gen_ai.request.model"], "empty request_model must NOT be emitted")
	assert.False(t, keys["server.address"], "empty server_address must NOT be emitted")
}

// TestInflight_ConcurrentBalance runs many parallel Inc/Dec pairs and
// confirms the package handles concurrent access without panics or
// race detector failures. The actual balance assertion (Inc count ==
// Dec count) is implicit — the OTel UpDownCounter would otherwise
// drift, but unit tests can't easily read the live cumulative value
// without exposing internal state. Stress + race is the right gate.
func TestInflight_ConcurrentBalance(t *testing.T) {
	m := GetInflightMetrics()
	if m == nil {
		t.Skip("meter init failed in test env")
	}
	const goroutines = 50
	const iterations = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			ctx := context.Background()
			for j := 0; j < iterations; j++ {
				tok := m.IncInflight(ctx, "chat", "vllm", "llama2:7b", "10.0.0.1")
				m.DecInflight(ctx, tok)
			}
		}(i)
	}
	wg.Wait()
}
