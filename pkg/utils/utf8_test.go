package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

type label string

func TestValidUTF8(t *testing.T) {
	t.Parallel()
	bad := "bad\xff"
	for name, value := range map[string]any{
		"nil":          nil,
		"string":       "ok \u2028 \ufffd",
		"nested":       map[string]any{"list": []any{"a", map[string]any{"b": label("c")}}, "n": 1.5},
		"string map":   map[string]string{"k": "v"},
		"struct":       struct{ Name string }{"ok"},
		"pointer":      new("ok"),
		"empty string": "",
	} {
		assert.True(t, ValidUTF8(value), name)
	}
	for name, value := range map[string]any{
		"string":       bad,
		"map key":      map[string]string{bad: "v"},
		"map value":    map[string]string{"k": bad},
		"nested any":   map[string]any{"list": []any{"a", map[string]any{"b": bad}}},
		"named string": map[string]any{"purpose": label(bad)},
		"struct":       struct{ Name string }{bad},
		"pointer":      &bad,
		"slice":        []string{"ok", bad},
	} {
		assert.False(t, ValidUTF8(value), name)
	}
}
