package normalizer

import (
	"strings"
	"testing"
)

func TestComposeStream(t *testing.T) {
	upper := StreamNormalizer(func(b []byte) []byte {
		return []byte(strings.ToUpper(string(b)))
	})
	exclaim := func(b []byte) []byte {
		return append(b, '!')
	}

	if ComposeStream(nil, nil) != nil {
		t.Error("both nil should produce nil transform")
	}
	single := ComposeStream(upper, nil)
	if got := string(single([]byte("abc"))); got != "ABC" {
		t.Errorf("upper-only: got %q", got)
	}
	chained := ComposeStream(upper, exclaim)
	if got := string(chained([]byte("abc"))); got != "ABC!" {
		t.Errorf("chain: got %q", got)
	}
}

func TestRegistryLookup(t *testing.T) {
	r := NewRegistry()
	r.Register("mlx", func(map[string]any) bool { return false })

	if r.Body("mlx") == nil {
		t.Error("mlx body normalizer not registered")
	}
	if r.Stream("mlx") == nil {
		t.Error("mlx stream normalizer not registered")
	}
	if r.Body("ollama") != nil {
		t.Error("ollama should have no body normalizer")
	}
	if r.Stream("ollama") != nil {
		t.Error("ollama should have no stream normalizer")
	}

	// nil-receiver safety
	var nilReg *Registry
	if nilReg.Body("mlx") != nil || nilReg.Stream("mlx") != nil {
		t.Error("nil receiver lookups should return nil")
	}

	// nil-fn register is a no-op (does not panic).
	r.Register("nope", nil)
	if r.Body("nope") != nil || r.Stream("nope") != nil {
		t.Error("nil fn should not be registered")
	}
}
