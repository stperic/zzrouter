package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestPythonRequirement_YAML_BareString guards the backward-compat path
// for provider YAMLs that predate the range schema. Regression bait:
// the first version of UnmarshalYAML used the yaml.v2 signature and was
// silently ignored by yaml.v3, so `python: "3.9"` parsed as a typed
// error and the entire provider failed to load.
func TestPythonRequirement_YAML_BareString(t *testing.T) {
	src := `python: "3.9"`
	var wrap struct {
		Python *PythonRequirement `yaml:"python"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(src), &wrap))
	require.NotNil(t, wrap.Python)
	assert.Equal(t, "3.9", wrap.Python.Min)
	assert.Equal(t, "", wrap.Python.Max)
}

func TestPythonRequirement_YAML_Struct(t *testing.T) {
	src := `
python:
  min: "3.9"
  max: "3.14"
`
	var wrap struct {
		Python *PythonRequirement `yaml:"python"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(src), &wrap))
	require.NotNil(t, wrap.Python)
	assert.Equal(t, "3.9", wrap.Python.Min)
	assert.Equal(t, "3.14", wrap.Python.Max)
}

// TestPythonRequirement_JSON_BareString guards the wire-format compat
// over /zzrouter/v1/providers — external callers may send a bare string.
func TestPythonRequirement_JSON_BareString(t *testing.T) {
	var p PythonRequirement
	require.NoError(t, json.Unmarshal([]byte(`"3.9"`), &p))
	assert.Equal(t, "3.9", p.Min)
	assert.Equal(t, "", p.Max)
}

func TestPythonRequirement_JSON_Struct(t *testing.T) {
	var p PythonRequirement
	require.NoError(t, json.Unmarshal([]byte(`{"min":"3.9","max":"3.14"}`), &p))
	assert.Equal(t, "3.9", p.Min)
	assert.Equal(t, "3.14", p.Max)
}

// TestPythonRequirement_YAML_RoundTrip_BareForm ensures we don't silently
// rewrite a legacy bare-string provider YAML into the struct form on
// marshal — config reviewers expect diffs to reflect intent, not schema
// upgrades they didn't ask for.
func TestPythonRequirement_YAML_RoundTrip_BareForm(t *testing.T) {
	p := PythonRequirement{Min: "3.9"}
	out, err := yaml.Marshal(p)
	require.NoError(t, err)
	// Bare-string emission is: "3.9\n" (with trailing newline from yaml.v3)
	assert.Equal(t, "\"3.9\"\n", string(out))
}

func TestPythonRequirement_JSON_RoundTrip_Struct(t *testing.T) {
	p := PythonRequirement{Min: "3.9", Max: "3.14"}
	out, err := json.Marshal(p)
	require.NoError(t, err)
	assert.JSONEq(t, `{"min":"3.9","max":"3.14"}`, string(out))
}
