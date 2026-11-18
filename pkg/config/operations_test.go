package config

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func operationFixture() OperationDefinition {
	return OperationDefinition{
		Version: 1, Description: "Inspect the selected engine.", Adapter: "run_json_read", Audience: "admin",
		Effect: "read", TargetScope: "run", Transport: "json", Runtimes: []string{"base"},
		Binding: OperationBinding{Method: "POST", Path: "/inspect"},
		InputSchema: OperationSchema{"type": "object", "required": []any{"values"}, "additionalProperties": false,
			"properties": map[string]any{"values": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/entry"}}},
			"$defs":      map[string]any{"entry": map[string]any{"type": "object", "required": []any{"text"}, "properties": map[string]any{"text": map[string]any{"type": "string"}}}}},
		OutputSchema: OperationSchema{"type": "object", "required": []any{"count"}, "properties": map[string]any{"count": map[string]any{"type": "integer", "minimum": 0}}},
	}
}

func operationService() ServiceConfig {
	return ServiceConfig{Mode: "on-demand", Protocol: ProtocolOpenAI,
		Runtime:      &AppRuntimeConfig{BasePort: 8100, PortRange: []int{8100, 8102}, Execution: ExecutionConfig{Type: "cli", Command: "engine"}},
		Capabilities: &AppCapabilities{WireEndpoints: []string{"chat_completions"}},
		Operations:   map[string]OperationDefinition{"inspect": operationFixture()}}
}

func TestOperationDefinitionValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*OperationDefinition)
	}{
		{"version", func(d *OperationDefinition) { d.Version = 2 }},
		{"adapter", func(d *OperationDefinition) { d.Adapter = "script" }},
		{"audience", func(d *OperationDefinition) { d.Audience = "user" }},
		{"effect", func(d *OperationDefinition) { d.Effect = "write" }},
		{"scope", func(d *OperationDefinition) { d.TargetScope = "provider" }},
		{"transport", func(d *OperationDefinition) { d.Transport = "sse" }},
		{"method", func(d *OperationDefinition) { d.Binding.Method = "DELETE" }},
		{"description", func(d *OperationDefinition) { d.Description = "\n" }},
		{"no runtime", func(d *OperationDefinition) { d.Runtimes = nil }},
		{"unknown runtime", func(d *OperationDefinition) { d.Runtimes = []string{"arbitrary-python"} }},
		{"duplicate runtime", func(d *OperationDefinition) { d.Runtimes = []string{"base", "base"} }},
		{"schema shape", func(d *OperationDefinition) { d.InputSchema = OperationSchema{"type": "unknown"} }},
		{"missing schema", func(d *OperationDefinition) { d.OutputSchema = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := operationService()
			definition := svc.Operations["inspect"]
			tc.mutate(&definition)
			svc.Operations["inspect"] = definition
			require.Error(t, (&AppsConfig{}).AddApp("engine", svc))
		})
	}
	for _, path := range []string{"https://engine.test/tokenize", "//engine.test/tokenize", "/../tokenize", "/a/../tokenize", "/tokenize?url=x", "/%2fadmin", "/${PATH}", "/{input}", "/a\\b", "/a#x", "/a\r\nHost:evil", "/" + strings.Repeat("a", 256)} {
		t.Run(path, func(t *testing.T) {
			svc := operationService()
			d := svc.Operations["inspect"]
			d.Binding.Path = path
			svc.Operations["inspect"] = d
			require.Error(t, (&AppsConfig{}).AddApp("engine", svc))
		})
	}
	for _, mode := range []string{"external", "cloud", "registry"} {
		t.Run(mode, func(t *testing.T) {
			svc := operationService()
			svc.Mode = mode
			require.ErrorContains(t, (&AppsConfig{}).AddApp("engine", svc), "on-demand owner")
		})
	}
}

func TestOperationSchemasAreBoundedAndLocal(t *testing.T) {
	var fetches atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		_, _ = w.Write([]byte(`{"type":"string"}`))
	}))
	t.Cleanup(upstream.Close)
	deep := map[string]any{"type": "string"}
	for range maxOperationSchemaDepth {
		deep = map[string]any{"not": deep}
	}
	many := map[string]any{}
	for i := range maxOperationSchemaNodes {
		many[fmt.Sprint(i)] = true
	}
	refs := map[string]any{}
	for i := range maxOperationSchemaReferences + 1 {
		refs[fmt.Sprint(i)] = map[string]any{"$ref": "#/$defs/entry"}
	}
	for _, tc := range []struct {
		name   string
		schema OperationSchema
	}{
		{"http", OperationSchema{"$ref": upstream.URL + "/schema"}},
		{"file", OperationSchema{"$ref": "file:///etc/passwd"}},
		{"relative resource", OperationSchema{"$ref": "other.json#/$defs/value"}},
		{"resource identity", OperationSchema{"$id": upstream.URL, "type": "object"}},
		{"unknown dialect", OperationSchema{"$schema": upstream.URL}},
		{"dynamic ref", OperationSchema{"$dynamicRef": "#/$defs/entry"}},
		{"missing ref", OperationSchema{"$ref": "#/$defs/missing"}},
		{"cycle", OperationSchema{"$defs": map[string]any{"entry": map[string]any{"$ref": "#/$defs/entry"}}}},
		{"legacy dependencies cycle", OperationSchema{"type": "object", "dependencies": map[string]any{"a": map[string]any{"properties": map[string]any{"b": map[string]any{"$ref": "#/dependencies/a"}}}}}},
		{"nested legacy dependencies", OperationSchema{"properties": map[string]any{"payload": map[string]any{"dependencies": map[string]any{"a": []any{"b"}}}}}},
		{"depth", OperationSchema(deep)},
		{"nodes", OperationSchema{"properties": many}},
		{"references", OperationSchema{"properties": refs, "$defs": map[string]any{"entry": map[string]any{"type": "string"}}}},
		{"size", OperationSchema{"description": strings.Repeat("x", maxOperationSchemaBytes)}},
	} {
		t.Run(tc.name, func(t *testing.T) { _, err := compileOperationSchema(tc.schema); require.Error(t, err) })
	}
	assert.Zero(t, fetches.Load(), "schema compilation must never fetch resources")
	_, err := compileOperationSchema(OperationSchema{"type": "object", "properties": map[string]any{"$ref": map[string]any{"type": "string"}, "dependencies": map[string]any{"type": "string"}}, "const": map[string]any{"$id": "ordinary JSON data", "dependencies": "ordinary JSON data"}})
	require.NoError(t, err, "schema keywords inside property names and data are not execution directives")
}

func TestCompiledOperationSnapshotAndRoundTrips(t *testing.T) {
	svc := operationService()
	svc.Runtime.Listener = &ListenerConfig{HostParameter: "host", DefaultHost: "127.0.0.1"}
	cfg := &AppsConfig{}
	require.NoError(t, cfg.AddApp("engine", svc))
	op, ok := cfg.Operation("engine.inspect")
	require.True(t, ok)
	assert.True(t, op.AppliesToRuntime(""))
	assert.True(t, op.AppliesToRuntime("engine"))
	assert.False(t, op.AppliesToRuntime("other-runtime"))
	valid := map[string]any{"values": []any{map[string]any{"text": "nested"}}}
	require.NoError(t, op.ValidateInput(valid))
	require.Error(t, op.ValidateInput(map[string]any{"values": []any{map[string]any{"text": 4}}}))
	require.NoError(t, op.ValidateOutput(map[string]any{"count": 2}))
	require.Error(t, op.ValidateOutput(map[string]any{"count": "2"}))

	svc.Operations["inspect"].InputSchema["type"] = "string"
	view, _ := cfg.LookupApp("engine")
	view.Operations["inspect"].InputSchema["type"] = "string"
	public := op.Definition()
	public.InputSchema["type"] = "string"
	public.Runtimes[0] = "other-runtime"
	require.NoError(t, op.ValidateInput(valid))
	assert.Equal(t, "object", op.Definition().InputSchema["type"])
	assert.True(t, op.AppliesToRuntime(""))

	dir := t.TempDir()
	require.NoError(t, cfg.SaveToDir(dir))
	loaded, err := LoadAppsConfig(dir)
	require.NoError(t, err)
	reloaded, ok := loaded.Operation(op.ID())
	require.True(t, ok)
	assert.Equal(t, op.Revision(), reloaded.Revision())
	loadedView, _ := loaded.LookupApp("engine")
	assert.Equal(t, svc.Runtime.Listener, loadedView.Runtime.Listener)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				assert.NoError(t, op.ValidateInput(valid))
				assert.Equal(t, "object", op.Definition().InputSchema["type"])
			}
		})
	}
	wg.Wait()
}

func TestOperationPublicationAndSyncValidation(t *testing.T) {
	dir := t.TempDir()
	cfg := &AppsConfig{}
	require.NoError(t, cfg.AddApp("engine", operationService()))
	require.NoError(t, cfg.SaveToDir(dir))
	store, err := NewAppsConfigStore(dir)
	require.NoError(t, err)
	before := store.Config()
	old, ok := before.Operation("engine.inspect")
	require.True(t, ok)
	path := filepath.Join(dir, "on-demand", "engine", "config.yaml")
	bytesBefore, err := os.ReadFile(path)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, yaml.Unmarshal(bytesBefore, &body))
	definition := body["operations"].(map[string]any)["inspect"].(map[string]any)
	definition["binding"].(map[string]any)["headers"] = map[string]any{"Authorization": "forbidden"}
	invalid, err := yaml.Marshal(body)
	require.NoError(t, err)
	changed, err := store.WriteProviderFiles("engine", KindOnDemand, ProviderFiles{Config: invalid})
	require.Error(t, err)
	assert.False(t, changed)
	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, bytesBefore, actual)
	assert.Same(t, before, store.Config())

	delete(definition["binding"].(map[string]any), "headers")
	definition["binding"].(map[string]any)["path"] = "/inspect-new"
	next, err := yaml.Marshal(body)
	require.NoError(t, err)
	changed, err = store.WriteProviderFiles("engine", KindOnDemand, ProviderFiles{Config: next})
	require.NoError(t, err)
	assert.True(t, changed)
	after, ok := store.Config().Operation(old.ID())
	require.True(t, ok)
	assert.NotEqual(t, old.Revision(), after.Revision())
	assert.NotEqual(t, before.Fingerprint(), store.Config().Fingerprint())
	assert.Equal(t, "/inspect", old.Definition().Binding.Path)
	assert.Equal(t, "/inspect-new", after.Definition().Binding.Path)
	var encoded OperationDefinition
	raw, err := json.Marshal(after.Definition())
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &encoded))
	assert.Equal(t, after.Definition(), encoded)
}

func TestOperationSchemaNumbersSurviveYAML(t *testing.T) {
	const raw = `{"type":"object","properties":{"decimal":{"type":"number","multipleOf":1.0},"large":{"const":9007199254740993}}}`
	var schema OperationSchema
	require.NoError(t, json.Unmarshal([]byte(raw), &schema))
	encoded, err := yaml.Marshal(schema)
	require.NoError(t, err)
	var restored OperationSchema
	require.NoError(t, yaml.Unmarshal(encoded, &restored))
	assert.Equal(t, schema, restored)
	before, err := json.Marshal(schema)
	require.NoError(t, err)
	after, err := json.Marshal(restored)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestListenerDeclarationValidationAndFeatureRoundTrip(t *testing.T) {
	for _, listener := range []*ListenerConfig{
		{}, {HostParameter: "--host"}, {HostEnvironment: "HOST=value"}, {DefaultHost: "http://engine.test"}, {DefaultHost: "user:secret@host"}, {DefaultHost: "host\nvalue"},
	} {
		require.Error(t, listener.Validate())
	}
	svc := operationService()
	svc.Features = map[string]Feature{"vision": {
		Runtime: "vision-engine", Execution: &ExecutionConfig{Type: "cli", Command: "vision-engine"},
		WireEndpoints: []string{"chat_completions"}, Listener: &ListenerConfig{HostEnvironment: "ENGINE_HOST", DefaultHost: "::1"},
	}}
	definition := svc.Operations["inspect"]
	definition.Runtimes = []string{"vision-engine"}
	svc.Operations["inspect"] = definition
	cfg := &AppsConfig{}
	require.NoError(t, cfg.AddApp("engine", svc))
	dir := t.TempDir()
	require.NoError(t, cfg.SaveToDir(dir))
	loaded, err := LoadAppsConfig(dir)
	require.NoError(t, err)
	view, _ := loaded.LookupApp("engine")
	assert.Equal(t, svc.Features["vision"].Listener, view.Features["vision"].Listener)
	op, ok := loaded.Operation("engine.inspect")
	require.True(t, ok)
	assert.False(t, op.AppliesToRuntime("engine"))
	assert.True(t, op.AppliesToRuntime("vision-engine"))
}
