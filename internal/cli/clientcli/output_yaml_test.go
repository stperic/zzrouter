package clientcli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type outputInner struct {
	Name string `json:"name"`
	Port string `json:"port"`
}

type outputOuter struct {
	outputInner
	Enabled bool     `json:"enabled"`
	Note    string   `json:"note,omitempty"`
	Tags    []string `json:"tags"`
}

// -o yaml describes the same document -o json does: an embedded struct's
// fields sit at the top, omitempty is honoured, keys keep JSON's names
// and order, and a string that reads as a number stays a string.
func TestYAMLLikeJSON(t *testing.T) {
	out, err := yamlLikeJSON(outputOuter{outputInner: outputInner{Name: "a", Port: "9090"}, Enabled: true, Tags: []string{"x"}})
	require.NoError(t, err)
	assert.Equal(t, "name: a\nport: \"9090\"\nenabled: true\ntags:\n    - x\n", string(out))
}
