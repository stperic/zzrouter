package schema_test

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/config/schema"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestFailurePatternClassSchema(t *testing.T) {
	for _, tc := range []struct {
		kind, name string
		validate   func([]byte) error
	}{
		{"on-demand", "vllm", schema.ValidateOnDemand},
		{"external", "ollama", schema.ValidateExternal},
	} {
		raw, err := templates.AppsFS.ReadFile("files/providers/" + tc.kind + "/" + tc.name + "/config.yaml")
		require.NoError(t, err)
		require.NoError(t, tc.validate(raw))
		var doc map[string]any
		require.NoError(t, yaml.Unmarshal(raw, &doc))
		patterns := doc["runtime"].(map[string]any)["failure_patterns"].([]any)
		patterns[0].(map[string]any)["class"] = "typo"
		raw, err = yaml.Marshal(doc)
		require.NoError(t, err)
		require.Error(t, tc.validate(raw))
		delete(patterns[0].(map[string]any), "class")
		raw, err = yaml.Marshal(doc)
		require.NoError(t, err)
		require.Error(t, tc.validate(raw))
	}
}
