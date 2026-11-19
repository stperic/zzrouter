package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestRegisteredToolValidatesOutputSchema(t *testing.T) {
	for _, tc := range []struct {
		name  string
		runID any
		valid bool
	}{
		{"valid", "run-1", true},
		{"invalid", 42, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDispatcher(func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected REST dispatch") })
			server := newServer(d.cfg)
			called := false
			registerTool(server, d.cfg, map[string][]string{}, &mcp.Tool{
				Name: "schema_contract", OutputSchema: objectSchema[runOutput]{},
			}, nil, func(_ dispatcher, _ context.Context, _ struct{}) (agentOutput, error) {
				called = true
				return agentOutput{fields: map[string]any{"run_id": tc.runID}}, nil
			})
			client, ctx := connectToolClient(t, d, server)
			result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "schema_contract", Arguments: map[string]any{}})
			require.True(t, called)
			if tc.valid {
				require.NoError(t, err)
				require.False(t, result.IsError)
				require.Equal(t, tc.runID, result.StructuredContent.(map[string]any)["run_id"])
			} else {
				require.ErrorContains(t, err, "validating tool output")
				require.Nil(t, result)
			}
		})
	}
}

func TestRegisteredToolPreservesRawNumbersAndSchemaDefaults(t *testing.T) {
	type input struct {
		Value json.RawMessage `json:"value"`
		Limit int             `json:"limit"`
	}
	d := testDispatcher(func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected REST dispatch") })
	server := newServer(d.cfg)
	calls := 0
	registerTool(server, d.cfg, map[string][]string{}, &mcp.Tool{Name: "raw_schema_contract", InputSchema: map[string]any{
		"type": "object", "required": []string{"value"}, "additionalProperties": false,
		"properties": map[string]any{"value": map[string]any{}, "limit": map[string]any{"type": "integer", "default": 2}},
	}}, nil, func(_ dispatcher, _ context.Context, in input) (agentOutput, error) {
		calls++
		return agentOutput{fields: map[string]any{"value": in.Value, "limit": in.Limit}, text: string(in.Value)}, nil
	})
	client, ctx := connectToolClient(t, d, server)
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "raw_schema_contract", Arguments: map[string]any{"value": json.RawMessage(`[9007199254740993,18446744073709551615,1.234567890123456789,2.0,2e0]`)}})
	require.NoError(t, err)
	require.False(t, result.IsError, "%+v", result)
	require.EqualValues(t, 2, result.StructuredContent.(map[string]any)["limit"])
	require.Equal(t, `[9007199254740993,18446744073709551615,1.234567890123456789,2.0,2e0]`, result.Content[0].(*mcp.TextContent).Text)
	for _, args := range []map[string]any{{}, {"value": true, "extra": true}, {"value": true, "limit": "bad"}} {
		result, err = client.CallTool(ctx, &mcp.CallToolParams{Name: "raw_schema_contract", Arguments: args})
		require.NoError(t, err)
		require.True(t, result.IsError)
	}
	require.Equal(t, 1, calls)
}

func TestRunToolParameterInputSchemas(t *testing.T) {
	for _, input := range []json.Marshaler{objectSchema[startInput]{}, objectSchema[previewInput]{}} {
		raw, err := input.MarshalJSON()
		require.NoError(t, err)
		var schema jsonschema.Schema
		require.NoError(t, json.Unmarshal(raw, &schema))
		parameters := schema.Properties["parameters"]
		require.Equal(t, "object", parameters.Type)
		require.Equal(t, []string{"string", "number", "boolean"}, parameters.AdditionalProperties.Types)
	}
}

func TestStableOutputSchemasValidateIDs(t *testing.T) {
	for _, tc := range []struct {
		name           string
		schema         json.Marshaler
		valid, invalid string
	}{
		{"run", objectSchema[runOutput]{}, `{"run_id":"r","job_id":"j"}`, `{"job_id":"j"}`},
		{"download", objectSchema[downloadOutput]{}, `{"deployment_id":"d","nodes":[{"node":"worker","job_id":"j"}]}`, `{"deployment_id":7}`},
		{"chat", objectSchema[chatOutput]{}, `{"probe":"chat","verdict":"verified","next_steps":"inspect logs","reply":"hello","model":"m","usage":{"total_tokens":4}}`, `{"probe":"chat","verdict":"verified","next_steps":"inspect logs","reply":"hello","model":"m","usage":{"total_tokens":"4"}}`},
		{"probe", objectSchema[chatOutput]{}, `{"probe":"tools","verdict":"verified","next_steps":"inspect logs","reply":"","model":"m","usage":{},"tool_call":"verified"}`, `{"probe":"tools","verdict":"verified","next_steps":"inspect logs","reply":"","model":"m","usage":{},"tool_call":"refused"}`},
		{"embed", objectSchema[chatOutput]{}, `{"probe":"embed","verdict":"verified","next_steps":"use embeddings","model":"m","dimensions":2,"usage":{}}`, `{"probe":"embed","verdict":"guessed","next_steps":"use embeddings","model":"m","dimensions":2,"usage":{}}`},
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

func TestAcceptedJobSchemasDoNotPredictExpiry(t *testing.T) {
	for _, output := range []json.Marshaler{objectSchema[runOutput]{}, objectSchema[downloadOutput]{}} {
		raw, err := output.MarshalJSON()
		require.NoError(t, err)
		var schema jsonschema.Schema
		require.NoError(t, json.Unmarshal(raw, &schema))
		require.NotContains(t, schema.Properties, "expires_at")
		if nodes := schema.Properties["nodes"]; nodes != nil {
			require.NotContains(t, nodes.Items.Properties, "expires_at")
		}
	}
}
