package schema

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config/runsettings"
	"github.com/stperic/zzrouter/pkg/httperr"
)

// Check reads the canonical string a launch passes: a JSON number or
// boolean arrives as its literal (an integral number as an integer
// literal), a string as itself. An int is only an integer literal.
func TestCheckCanonicalStrings(t *testing.T) {
	shapes := map[string]ParamShape{
		"ctx":                          {Kind: ParamInt, Min: fptr(1), Auto: "0"},
		"layers":                       {Kind: ParamInt},
		"fraction":                     {Kind: ParamFloat, Min: fptr(0), Max: fptr(1)},
		"flash":                        {Kind: ParamBool},
		"pooling":                      {Kind: ParamString, Enum: []string{"mean", "cls"}},
		"template":                     {Kind: ParamAsset},
		runsettings.KeepAliveParameter: {Kind: ParamString},
	}
	for _, tc := range []struct {
		key, value string
		code       httperr.ParamErrorCode
	}{
		{"ctx", "32768", ""},
		{"ctx", "32768.0", httperr.CodeWrongType},
		{"ctx", "3.2768e+04", httperr.CodeWrongType},
		{"ctx", "10000000000000000000", httperr.CodeOutOfRange},
		{"ctx", "9223372036854775808", httperr.CodeOutOfRange},
		{"ctx", "1e81", httperr.CodeOutOfRange},
		{"ctx", "-1e999999", httperr.CodeOutOfRange},
		{"ctx", "auto", ""},
		{"ctx", "0", httperr.CodeOutOfRange},
		{"ctx", "1.5", httperr.CodeWrongType},
		{"ctx", "Inf", httperr.CodeWrongType},
		{"layers", "auto", httperr.CodeWrongType},
		{"fraction", "0.9", ""},
		{"fraction", "1.5", httperr.CodeOutOfRange},
		{"fraction", "NaN", httperr.CodeWrongType},
		{"flash", "true", ""},
		{"flash", "1", httperr.CodeWrongType},
		{"pooling", "mean", ""},
		{"pooling", "max", httperr.CodeCoercionFailed},
		{"template", "chat.jinja", ""},
		{"template", "../chat.jinja", httperr.CodeInvalidValue},
		{runsettings.KeepAliveParameter, "-1", ""},
		{runsettings.KeepAliveParameter, "soon", httperr.CodeCoercionFailed},
		{"context_length", "32768", httperr.CodeUnknownFlag},
	} {
		issue := Check(tc.key, tc.value, shapes)
		if tc.code == "" {
			assert.Nil(t, issue, "%s=%s", tc.key, tc.value)
			continue
		}
		if assert.NotNil(t, issue, "%s=%s", tc.key, tc.value) {
			assert.Equal(t, string(tc.code), issue.Code, "%s=%s", tc.key, tc.value)
			assert.Equal(t, tc.key, issue.Key)
		}
	}
}

// A negatable bool has one key: its --no- spelling is unknown, and the
// refusal names the key and value that pass it.
func TestCheckPointsANegatedKeyAtItsBool(t *testing.T) {
	shapes := map[string]ParamShape{
		"mmap":    {Kind: ParamBool, Negatable: true},
		"cpu-moe": {Kind: ParamBool},
		"no-host": {Kind: ParamBool},
	}
	issue := Check("no-mmap", "true", shapes)
	if assert.NotNil(t, issue) {
		assert.Equal(t, string(httperr.CodeUnknownFlag), issue.Code)
		assert.Contains(t, issue.Message, "set mmap: false")
	}
	issue = Check("no-cpu-moe", "true", shapes)
	if assert.NotNil(t, issue) {
		assert.NotContains(t, issue.Message, "set cpu-moe", "a plain bool has no --no- spelling")
	}
	assert.Nil(t, Check("no-host", "true", shapes), "a flag spelled only --no-x is its own bool")
	assert.Nil(t, Check("mmap", "false", shapes))
}

// An enum bounds a value of any type, as vLLM's max-lora-rank set does.
func TestCheckEnumBoundsNumbers(t *testing.T) {
	shapes := map[string]ParamShape{"rank": {Kind: ParamInt, Enum: []string{"8", "16"}}}
	assert.Nil(t, Check("rank", "16", shapes))
	issue := Check("rank", "12", shapes)
	if assert.NotNil(t, issue) {
		assert.Equal(t, string(httperr.CodeCoercionFailed), issue.Code)
	}
}

// A numeric enum holds its entries as a value is written, so the 1 a
// request's 1.0 becomes is one of them; an entry that is no number fails.
func TestMerge_NumericEnumIsCanonical(t *testing.T) {
	y, err := LoadYAMLSchema([]byte("provider: x\nparameters:\n  scale: {type: float, enum: [0.5, 1.0, 2e0]}\n  rank: {type: int, enum: [8, 16.0]}\n  mode: {type: string, enum: [\"1.0\"]}\n"))
	require.NoError(t, err)
	merged, errs := Merge("x", y)
	require.Empty(t, errs)
	assert.Equal(t, []string{"0.5", "1", "2"}, merged.Parameters["scale"].Enum)
	assert.Equal(t, []string{"8", "16"}, merged.Parameters["rank"].Enum)
	assert.Equal(t, []string{"1.0"}, merged.Parameters["mode"].Enum, "a string enum is compared as written")
	assert.Nil(t, Check("scale", "1", merged.Parameters))

	y, err = LoadYAMLSchema([]byte("provider: x\nparameters:\n  scale: {type: float, enum: [low]}\n"))
	require.NoError(t, err)
	_, errs = Merge("x", y)
	require.Len(t, errs, 1)
	assert.ErrorContains(t, errs[0], `enum entry "low" is not a number`)
}
