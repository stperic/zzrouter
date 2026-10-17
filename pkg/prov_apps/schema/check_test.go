package schema

import (
	"testing"

	"github.com/stretchr/testify/assert"

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
