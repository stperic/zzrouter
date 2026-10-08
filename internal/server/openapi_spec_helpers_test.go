package server

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// loadSpecTree parses openapi.yaml as a generic tree. The typed
// openAPIFile the drift tests use cannot carry schemas: this gate has
// to follow $ref and walk arbitrary shapes.
func loadSpecTree(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("openapi.yaml")
	if err != nil {
		alt := filepath.Join("internal", "server", "openapi.yaml")
		data, err = os.ReadFile(alt)
		if err != nil {
			t.Fatalf("read openapi.yaml: %v", err)
		}
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	return doc
}

func mapAt(node any, key string) (map[string]any, bool) {
	m, ok := node.(map[string]any)
	if !ok {
		return nil, false
	}
	child, ok := m[key].(map[string]any)
	return child, ok
}
