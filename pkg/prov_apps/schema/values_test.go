package schema

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// A number keeps its literal, never passing through floating point, unless
// it is integral: then it is an integer literal, the one form every engine's
// integer parser takes. A literal beyond the bound stays as written, so
// 1e999999 never reaches big.Rat.
func TestParameterValuesPreservesScalars(t *testing.T) {
	var params ParameterValues
	require.NoError(t, json.Unmarshal([]byte(`{"string":"auto","empty":"","escaped":"a\nb","int":16384,"large":9007199254740993,"decimal":0.1234567890123456789,"exponent":1e3,"integral":32768.0,"huge":1e30,"unbounded":1e999999,"negative":-1,"zero":-0,"numeric string":"3.2768e+04","true":true,"false":false}`), &params))
	require.Equal(t, ParameterValues{
		"string": "auto", "empty": "", "escaped": "a\nb", "int": "16384", "large": "9007199254740993",
		"decimal": "0.1234567890123456789", "exponent": "1000", "integral": "32768", "huge": "1000000000000000000000000000000", "unbounded": "1e999999",
		"negative": "-1", "zero": "-0", "numeric string": "3.2768e+04", "true": "true", "false": "false",
	}, params)
	encoded, err := json.Marshal(params)
	require.NoError(t, err)
	var strings map[string]string
	require.NoError(t, json.Unmarshal(encoded, &strings))
	require.Equal(t, map[string]string(params), strings)
}

func TestParameterValuesRejectsNonScalars(t *testing.T) {
	for _, value := range []string{"null", "{}", "[]", `["value"]`} {
		t.Run(value, func(t *testing.T) {
			params := ParameterValues{"existing": "unchanged"}
			err := json.Unmarshal([]byte(`{"bad":`+value+`}`), &params)
			var invalid *ParameterValueError
			require.ErrorAs(t, err, &invalid)
			require.Equal(t, "bad", invalid.Key)
			require.Contains(t, err.Error(), "string, number or boolean")
			require.Equal(t, ParameterValues{"existing": "unchanged"}, params)
		})
	}
}
