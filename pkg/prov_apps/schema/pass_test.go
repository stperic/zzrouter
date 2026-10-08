package schema

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMerge_AssetPass(t *testing.T) {
	y := &YAMLSchema{Parameters: map[string]YAMLParam{
		"by-content": {Type: "asset", Pass: "content"},
		"by-path":    {Type: "asset", Pass: "path"},
		"default":    {Type: "asset"},
	}}
	got, errs := Merge("eng", y)
	require.Empty(t, errs)
	assert.Equal(t, AssetPassContent, got.Parameters["by-content"].Pass)
	assert.Equal(t, AssetPassPath, got.Parameters["by-path"].Pass)
	assert.Empty(t, got.Parameters["default"].Pass, "empty means path")
}

// pass means nothing to a non-asset kind, and a typo would otherwise hand
// the engine a path where it wanted text.
func TestMerge_AssetPassRefusals(t *testing.T) {
	for name, yp := range map[string]YAMLParam{
		"not an asset": {Type: "string", Pass: "content"},
		"unknown mode": {Type: "asset", Pass: "inline"},
	} {
		got, errs := Merge("eng", &YAMLSchema{Parameters: map[string]YAMLParam{"k": yp}})
		assert.Len(t, errs, 1, name)
		assert.NotContains(t, got.Parameters, "k", name)
	}
}
