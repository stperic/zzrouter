package mcpserver

import (
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/require"
)

func TestSchemaObjectsPreservesBooleanValues(t *testing.T) {
	input := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{"patch": true, "enabled": map[string]any{"type": "boolean", "default": false, "readOnly": true, "enum": []any{false, true}}},
	}
	raw, err := json.Marshal(schemaObjects(input))
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"object","additionalProperties":{"not":{}},"properties":{"patch":{},"enabled":{"type":"boolean","default":false,"readOnly":true,"enum":[false,true]}}}`, string(raw))
}

func TestStableOutputSchemasValidateIDs(t *testing.T) {
	for _, tc := range []struct {
		name           string
		schema         json.Marshaler
		valid, invalid string
	}{
		{"run", objectSchema[runOutput]{}, `{"run_id":"r","job_id":"j"}`, `{"job_id":"j"}`},
		{"download", objectSchema[downloadOutput]{}, `{"deployment_id":"d","nodes":[{"node":"worker","job_id":"j"}]}`, `{"deployment_id":7}`},
		{"chat", objectSchema[chatOutput]{}, `{"reply":"hello","model":"m","usage":{"total_tokens":4}}`, `{"reply":"hello","model":"m","usage":{"total_tokens":"4"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := tc.schema.MarshalJSON()
			require.NoError(t, err)
			var schema jsonschema.Schema
			require.NoError(t, json.Unmarshal(raw, &schema))
			resolved, err := schema.Resolve(nil)
			require.NoError(t, err)
			for _, value := range []struct {
				raw   string
				valid bool
			}{{tc.valid, true}, {tc.invalid, false}} {
				var decoded any
				require.NoError(t, json.Unmarshal([]byte(value.raw), &decoded))
				if value.valid {
					require.NoError(t, resolved.Validate(decoded))
				} else {
					require.Error(t, resolved.Validate(decoded))
				}
			}
		})
	}
}
