package schema

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func operatorConfig(t *testing.T, body string) config.ServiceConfig {
	t.Helper()
	var cfg config.ServiceConfig
	require.NoError(t, yaml.Unmarshal([]byte(body), &cfg))
	return cfg
}

func TestOperatorDeclarations(t *testing.T) {
	shipped := &YAMLSchema{Parameters: map[string]YAMLParam{
		"cache-kind": {Type: "string", Enum: []string{"old"}, KVCacheDtype: "k"},
		"count":      {Type: "int", Min: new(2.0), Max: new(8.0)},
	}, Endpoints: map[string]YAMLEndpoint{"embeddings": {Parameters: map[string]YAMLParam{"cache-kind": {Enum: []string{"endpoint"}}, "endpoint-only": {Type: "int", Enum: []string{"2"}}}}}}
	for _, tc := range []struct {
		name, body string
		accepted   []string
		problems   map[string]string
	}{
		{"new shapes", "schema:\n  future_flag: {type: bool, negatable: true}\n  literal-auto: {type: int, auto: '-1'}\n  template: {type: asset, pass: content}", []string{"future-flag", "literal-auto", "template"}, nil},
		{"widen enum and range", "schema:\n  cache-kind: {enum: [old, new], description: operator}\n  count: {min: 1, max: 10}", []string{"cache-kind", "count"}, nil},
		{"endpoint-only shipped", "schema:\n  endpoint-only: {type: int, enum: ['1', '2']}", nil, map[string]string{"schema.endpoint-only": "invalid_value"}},
		{"endpoint-only conflict", "schema:\n  endpoint-only: {type: string}", nil, map[string]string{"schema.endpoint-only": "invalid_value"}},
		{"invalid keys", "schema:\n  'bad key --x=1': {type: int}\n  '-lead': {type: bool}\n  sibling: {type: string}", []string{"sibling"}, map[string]string{"schema.bad key --x=1": "invalid_value", "schema.-lead": "invalid_value"}},
		{"reversed range", "schema:\n  bad: {type: int, min: 5, max: 1}\n  sibling: {type: string}", []string{"sibling"}, map[string]string{"schema.bad.min": "invalid_value"}},
		{"reversed shipped range", "schema:\n  count: {min: 9}", nil, map[string]string{"schema.count.min": "invalid_value"}},
		{"owned spine", "schema:\n  max-model-len: {description: forbidden}", nil, map[string]string{"schema.max-model-len": "invalid_value"}},
		{"owned settings", "schema:\n  keep_alive: {type: string}", nil, map[string]string{"schema.keep_alive": "invalid_value"}},
		{"owned feature", "features:\n  vision: {flag: side_flag}\nschema:\n  side-flag: {type: string}", nil, map[string]string{"schema.side-flag": "invalid_value"}},
		{"type conflict", "schema:\n  cache-kind: {type: int, enum: ['1']}", nil, map[string]string{"schema.cache-kind.type": "invalid_value"}},
		{"two spellings", "schema:\n  future_flag: {type: bool}\n  future-flag: {type: bool}\n  sibling: {type: string}", []string{"sibling"}, map[string]string{"schema.future_flag": "duplicate_flag", "schema.future-flag": "duplicate_flag"}},
		{"bad entry alone", "schema:\n  bad: {type: invented}\n  sibling: {type: string}", []string{"sibling"}, map[string]string{"schema.bad": "invalid_value"}},
		{"bad pass", "schema:\n  bad: {type: string, pass: content}", nil, map[string]string{"schema.bad": "invalid_value"}},
		{"bad negatable", "schema:\n  bad: {type: string, negatable: true}", nil, map[string]string{"schema.bad": "invalid_value"}},
		{"shipped fields", "schema:\n  cache-kind: {type: string, pass: '', auto: '', negatable: false, enum: [new]}", []string{"cache-kind"}, map[string]string{"schema.cache-kind.type": "invalid_value", "schema.cache-kind.pass": "invalid_value", "schema.cache-kind.auto": "invalid_value", "schema.cache-kind.negatable": "invalid_value"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := operatorConfig(t, tc.body)
			got, errs := Merge("vllm", shipped, cfg)
			require.Empty(t, errs)
			problems := map[string]string{}
			for _, p := range got.Problems {
				problems[p.Key] = p.Code
			}
			assert.Equal(t, len(tc.problems), len(problems))
			for path, code := range tc.problems {
				assert.Equal(t, code, problems[path], path)
			}
			for _, key := range tc.accepted {
				assert.True(t, got.Parameters[key].Operator, key)
			}
			for _, key := range []string{"future-flag", "sibling", "bad", "literal-auto", "template"} {
				if _, ok := got.Parameters[key]; ok {
					assert.Contains(t, tc.accepted, key)
				}
			}
			switch tc.name {
			case "invalid keys":
				assert.NotContains(t, got.Parameters, "bad key --x=1")
				assert.NotContains(t, got.Parameters, "-lead")
			case "reversed shipped range":
				assert.Equal(t, 2.0, *got.Parameters["count"].Min)
				assert.Equal(t, 8.0, *got.Parameters["count"].Max)
			case "endpoint-only shipped", "endpoint-only conflict":
				require.Len(t, got.Problems, 1)
				assert.Contains(t, got.Problems[0].Message, "embeddings")
				assert.NotContains(t, got.Parameters, "endpoint-only")
				assert.Equal(t, ParamInt, got.ForEndpoint("embeddings")["endpoint-only"].Kind)
				assert.Equal(t, []string{"2"}, got.ForEndpoint("embeddings")["endpoint-only"].Enum)
				assert.False(t, got.ForEndpoint("embeddings")["endpoint-only"].Operator)
				assert.Nil(t, Check("endpoint-only", "2", got.ForEndpoint("embeddings")))
				assert.NotNil(t, Check("endpoint-only", "1", got.ForEndpoint("embeddings")))
				assert.Equal(t, string(httperr.CodeUnknownFlag), Check("endpoint-only", "2", got.Parameters).Code)
			case "new shapes":
				assert.Equal(t, "future_flag", got.Parameters["future-flag"].Flag)
				assert.True(t, got.Parameters["future-flag"].Negatable)
				assert.Equal(t, "-1", got.Parameters["literal-auto"].Auto)
				assert.Equal(t, AssetPassContent, got.Parameters["template"].Pass)
				assert.Nil(t, Check("literal-auto", "auto", got.Parameters))
			case "widen enum and range":
				assert.Equal(t, []string{"old", "new"}, got.Parameters["cache-kind"].Enum)
				assert.Equal(t, "k", got.Parameters["cache-kind"].KVCacheDtype)
				assert.Equal(t, "operator", got.Parameters["cache-kind"].Description)
				assert.Equal(t, 1.0, *got.Parameters["count"].Min)
				assert.Equal(t, 10.0, *got.Parameters["count"].Max)
				assert.Equal(t, []string{"old", "new"}, got.ForEndpoint("embeddings")["cache-kind"].Enum)
				assert.True(t, got.ForEndpoint("embeddings")["cache-kind"].Operator)
			case "type conflict":
				assert.Equal(t, []string{"old"}, got.Parameters["cache-kind"].Enum)
			case "two spellings":
				assert.Equal(t, string(httperr.CodeUnknownFlag), Check("future_flag", "true", got.Parameters).Code)
			}
		})
	}
}

func TestOperatorReleasePromotion(t *testing.T) {
	cfg := operatorConfig(t, "schema:\n  future: {type: int, min: 1, max: 10, enum: ['2', '4'], description: operator, auto: auto}")
	initial, errs := Merge("vllm", nil, cfg)
	require.Empty(t, errs)
	require.Empty(t, initial.Problems)
	shipped := &YAMLSchema{Parameters: map[string]YAMLParam{"future": {Type: "int", Min: new(3.0), Max: new(5.0), Enum: []string{"3"}, Description: "release", Auto: "-1"}}}
	got, errs := Merge("vllm", shipped, cfg)
	require.Empty(t, errs)
	require.Len(t, got.Problems, 2)
	assert.Equal(t, "schema.future.auto", got.Problems[0].Key)
	assert.Equal(t, "schema.future.type", got.Problems[1].Key)
	shape := got.Parameters["future"]
	assert.True(t, shape.Operator)
	assert.Equal(t, 1.0, *shape.Min)
	assert.Equal(t, 10.0, *shape.Max)
	assert.Equal(t, []string{"2", "4"}, shape.Enum)
	assert.Equal(t, "operator", shape.Description)
	assert.Equal(t, "-1", shape.Auto)
	assert.Nil(t, Check("future", "2", got.Parameters))
	assert.NotNil(t, Check("future", "3", got.Parameters))
}
