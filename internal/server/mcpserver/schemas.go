package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	commonmcp "github.com/stperic/common/mcp"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

// Tool schemas follow from Go types and literals alone, so the keys are finite
// and every server reuses the published result; nothing writes a Resolved once it
// is resolved, and schema JSON leaves the cache only as a copy.
var (
	typeSchemas     sync.Map // reflect.Type -> []byte
	resolvedSchemas sync.Map // schema JSON -> *jsonschema.Resolved
)

// cached returns key's shared value, building a candidate on a cache miss.
func cached[V any](m *sync.Map, key any, build func() (V, error)) (V, error) {
	if v, ok := m.Load(key); ok {
		if value, ok := v.(V); ok {
			return value, nil
		}
	}
	value, err := build()
	if err != nil {
		return value, err
	}
	if v, loaded := m.LoadOrStore(key, value); loaded {
		if shared, ok := v.(V); ok {
			return shared, nil
		}
	}
	return value, nil
}

func resolveToolSchema(value any) (*jsonschema.Resolved, error) {
	if value == nil {
		return nil, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return cached(&resolvedSchemas, string(raw), func() (*jsonschema.Resolved, error) {
		var declared jsonschema.Schema
		if err := json.Unmarshal(raw, &declared); err != nil {
			return nil, err
		}
		return declared.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	})
}

func applyToolSchema(raw []byte, declared *jsonschema.Resolved, input bool) ([]byte, error) {
	if declared == nil {
		return raw, nil
	}
	if input && (len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))) {
		raw = []byte(`{}`)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	_, object := value.(map[string]any)
	if input && !object {
		return nil, fmt.Errorf("arguments must be a JSON object")
	}
	if !input && value == nil && declared.Schema().Type == "object" {
		value = map[string]any{}
		object = true
	}
	if object {
		if err := declared.ApplyDefaults(&value); err != nil {
			return nil, err
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	// The validator classifies json.Number as a string. Validate its ordinary
	// JSON projection while retaining the number-preserving tree for execution.
	var validation any
	if err := json.Unmarshal(encoded, &validation); err != nil {
		return nil, err
	}
	if err := declared.Validate(validation); err != nil {
		return nil, err
	}
	return encoded, nil
}

func decodeToolInput[In any](raw []byte) (In, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	err := decoder.Decode(&value)
	var result In
	if err != nil {
		return result, err
	}
	value = typedIntegerSpellings(reflect.TypeFor[In](), value)
	encoded, err := json.Marshal(value)
	if err == nil {
		err = json.Unmarshal(encoded, &result)
	}
	return result, err
}

// Normalize actual Go integer fields; raw provider JSON retains its exact numbers.
func typedIntegerSpellings(kind reflect.Type, value any) any {
	for kind.Kind() == reflect.Pointer {
		kind = kind.Elem()
	}
	if kind == reflect.TypeFor[json.RawMessage]() {
		return value
	}
	switch kind.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return value
		}
		for index := range kind.NumField() {
			field := kind.Field(index)
			tag := strings.Split(field.Tag.Get("json"), ",")[0]
			if tag == "-" {
				continue
			}
			if field.Anonymous && tag == "" {
				typedIntegerSpellings(field.Type, object)
				continue
			}
			if field.PkgPath != "" {
				continue
			}
			if tag == "" {
				tag = field.Name
			}
			if child, present := object[tag]; present {
				object[tag] = typedIntegerSpellings(field.Type, child)
			}
		}
	case reflect.Slice, reflect.Array:
		if array, ok := value.([]any); ok {
			for index, child := range array {
				array[index] = typedIntegerSpellings(kind.Elem(), child)
			}
		}
	case reflect.Map:
		if object, ok := value.(map[string]any); ok {
			for key, child := range object {
				object[key] = typedIntegerSpellings(kind.Elem(), child)
			}
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if number, ok := value.(json.Number); ok {
			return canonicalIntegerSpelling(number)
		}
	}
	return value
}

func canonicalIntegerSpelling(number json.Number) json.Number {
	raw := number.String()
	if len(raw) > 64 || !strings.ContainsAny(raw, ".eE") {
		return number
	}
	bounded, err := strconv.ParseFloat(raw, 64)
	// Bound Rat allocation by finite float range; never expand an arbitrary exponent.
	if err != nil {
		return number
	}
	if bounded == 0 {
		mantissa := strings.Split(strings.ToLower(raw), "e")[0]
		if strings.Trim(mantissa, "0.+-") == "" {
			return json.Number("0")
		}
		return number
	}
	if exact, ok := new(big.Rat).SetString(raw); ok && exact.IsInt() && (exact.Num().IsInt64() || exact.Num().IsUint64()) {
		return json.Number(exact.Num().String())
	}
	return number
}

// objectSchema preserves validation while avoiding SDK boolean subschemas on the wire.
type objectSchema[T any] struct{}

func (objectSchema[T]) MarshalJSON() ([]byte, error) {
	encoded, err := cached(&typeSchemas, reflect.TypeFor[T](), func() ([]byte, error) {
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
	})
	return bytes.Clone(encoded), err
}

type runOutput struct {
	AdmissionWarning *instance.AdmissionWarning `json:"admission_warning,omitempty"`

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
