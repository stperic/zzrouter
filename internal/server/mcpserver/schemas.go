package mcpserver

import (
	"encoding/json"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
	commonmcp "github.com/stperic/common/mcp"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

// objectSchema preserves validation while avoiding SDK boolean subschemas on the wire.
type objectSchema[T any] struct{}

func (objectSchema[T]) MarshalJSON() ([]byte, error) {
	schema, err := commonmcp.SchemaFor[T](map[reflect.Type]*jsonschema.Schema{
		reflect.TypeFor[schema.ParameterValues](): {Type: "object", AdditionalProperties: &jsonschema.Schema{Types: []string{"string", "number", "boolean"}}},
		reflect.TypeFor[toolCallOutcome]():        {Type: "string", Enum: []any{toolCallVerified, toolCallParserMissing, toolCallTruncated, toolCallInconclusive}},
		reflect.TypeFor[probeKind]():              {Type: "string", Enum: []any{probeChat, probeTools, probeEmbed, probeVision}},
		reflect.TypeFor[probeVerdict]():           {Type: "string", Enum: []any{probeVerified, probeNotSupported, probeInconclusive}},
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(schema)
}

type runOutput struct {
	Parameters  map[string]string `json:"parameters,omitempty"`
	Pinned      bool              `json:"pinned,omitempty"`
	BaseURL     string            `json:"base_url,omitempty"`
	RunID       string            `json:"run_id"`
	OldRunID    string            `json:"old_run_id,omitempty"`
	JobID       string            `json:"job_id,omitempty"`
	DeployJobID string            `json:"deploy_job_id,omitempty"`
	Node        string            `json:"node,omitempty"`
	Provider    string            `json:"provider,omitempty"`
	Model       string            `json:"model,omitempty"`
	Status      string            `json:"status,omitempty"`
	Port        int               `json:"port,omitempty"`
}
type downloadOutput struct {
	DeploymentID string         `json:"deployment_id"`
	Model        string         `json:"model,omitempty"`
	File         string         `json:"file,omitempty"`
	Status       string         `json:"status,omitempty"`
	RestartJobID string         `json:"restart_job_id,omitempty"`
	Nodes        []downloadNode `json:"nodes,omitempty"`
}
type downloadNode struct {
	Node   string `json:"node,omitempty"`
	JobID  string `json:"job_id,omitempty"`
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}
type probeOutput struct {
	Probe     probeKind    `json:"probe"`
	Verdict   probeVerdict `json:"verdict" jsonschema:"Evidence from this request only, not a stored capability claim."`
	NextSteps string       `json:"next_steps"`
}
type chatOutput struct {
	probeOutput
	Dimensions int             `json:"dimensions,omitempty"`
	ToolCall   toolCallOutcome `json:"tool_call,omitempty" jsonschema:"Request-local tool probe outcome: verified, parser_missing, truncated or inconclusive."`
	Reply      string          `json:"reply,omitempty"`
	Model      string          `json:"model"`
	Usage      tokenUsage      `json:"usage"`
}
type tokenUsage struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
}
