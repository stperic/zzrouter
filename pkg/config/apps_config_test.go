package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/config/templates"
)

func firstLine(b []byte) string {
	if i := strings.IndexByte(string(b), '\n'); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// ============================================================================
// AppsConfig Tests
// ============================================================================

// providersFixture builds an AppsConfig directly from typed providers,
// bypassing AddApp's per-kind Validate so unit tests can exercise the
// enabled/mode/kind dispatch with minimal field setup.
func providersFixture(ps map[string]Provider) *AppsConfig {
	return &AppsConfig{providers: ps}
}

func TestAppsConfig_IsAppEnabled_Exists(t *testing.T) {
	cfg := providersFixture(map[string]Provider{
		"ollama": &ExternalProvider{Name: "ollama", Enabled: new(true)},
		"vllm":   &OnDemandProvider{Name: "vllm", Enabled: new(false)},
	})

	if !cfg.IsAppEnabled("ollama") {
		t.Error("ollama should be enabled")
	}
	if cfg.IsAppEnabled("vllm") {
		t.Error("vllm should be disabled")
	}
}

func TestAppsConfig_IsAppEnabled_NotExists(t *testing.T) {
	cfg := providersFixture(map[string]Provider{})

	// Non-existent apps default to enabled
	if !cfg.IsAppEnabled("nonexistent") {
		t.Error("non-existent app should default to enabled")
	}
}

func TestAppsConfig_GetEnabledApps(t *testing.T) {
	cfg := providersFixture(map[string]Provider{
		"ollama":   &ExternalProvider{Name: "ollama", Enabled: new(true)},
		"vllm":     &OnDemandProvider{Name: "vllm", Enabled: new(false)},
		"llamacpp": &OnDemandProvider{Name: "llamacpp", Enabled: nil}, // nil = auto-discovery
	})

	enabled := cfg.GetEnabledApps()

	if len(enabled) != 1 {
		t.Errorf("Expected 1 enabled app, got %d", len(enabled))
	}

	hasOllama := false
	for _, name := range enabled {
		if name == "ollama" {
			hasOllama = true
		}
		if name == "vllm" {
			t.Error("vllm should NOT be in enabled list")
		}
	}
	if !hasOllama {
		t.Error("ollama should be in enabled list")
	}
}

func TestAppsConfig_GetOnDemandApps(t *testing.T) {
	cfg := providersFixture(map[string]Provider{
		"ollama":   &ExternalProvider{Name: "ollama", Enabled: new(true)},
		"vllm":     &OnDemandProvider{Name: "vllm", Enabled: new(true)},
		"llamacpp": &OnDemandProvider{Name: "llamacpp", Enabled: new(true)},
	})

	onDemand := cfg.GetOnDemandApps()

	if len(onDemand) != 2 {
		t.Errorf("Expected 2 on-demand apps, got %d", len(onDemand))
	}
}

// TestLoadAppsConfig_DirNotFound covers the common deployment failure
// where no providers/ directory has been initialized yet.
func TestLoadAppsConfig_DirNotFound(t *testing.T) {
	_, err := LoadAppsConfig("/nonexistent/path/providers")
	if err == nil {
		t.Error("LoadAppsConfig should return error for non-existent directory")
	}
}

// TestLoadAppsConfig_InvalidYAML — write a syntactically broken YAML
// file into the on-demand subdir and expect a parse error.
func TestLoadAppsConfig_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	require := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	require(os.MkdirAll(filepath.Join(dir, "on-demand"), 0755))
	require(os.WriteFile(filepath.Join(dir, "on-demand", "broken.yaml"),
		[]byte("invalid: yaml: : :\n[broken"), 0644))

	_, err := LoadAppsConfig(dir)
	if err == nil {
		t.Error("LoadAppsConfig should return error for invalid YAML")
	}
}

// TestLoadAppsConfig_MissingApps — empty providers/ dir should fail
// with the "at least one provider" guard.
func TestLoadAppsConfig_MissingApps(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	_, err := LoadAppsConfig(dir)
	if err == nil {
		t.Error("LoadAppsConfig should require at least one provider")
	}
}

// TestLoadAppsConfig_ValidConfig — install embedded defaults into a
// temp dir and verify expected providers load with correct kinds.
func TestLoadAppsConfig_ValidConfig(t *testing.T) {
	dir := t.TempDir()
	if err := templates.InstallDefaults(dir); err != nil {
		t.Fatalf("InstallDefaults: %v", err)
	}

	cfg, err := LoadAppsConfig(dir)
	if err != nil {
		t.Fatalf("LoadAppsConfig() error = %v", err)
	}

	// Spot check known providers.
	for _, name := range []string{"vllm", "ollama", "openai", "huggingface"} {
		if cfg.Find(name) == nil {
			t.Errorf("expected provider %q to load from defaults", name)
		}
	}
}

// vllmTestProvider returns a minimal-but-valid on-demand provider that
// passes per-kind Validate, suitable for fixtures that exercise mutation
// paths (SaveAppParameters, SaveModelDefaults) without caring about
// runtime details.
func vllmTestProvider() *OnDemandProvider {
	return &OnDemandProvider{
		Name:     "vllm",
		Enabled:  new(true),
		Protocol: ProtocolOpenAI,
		Runtime: OnDemandRuntime{
			PortRange: []int{8000, 8009},
			BasePort:  8000,
			Execution: ExecutionConfig{Type: "cli", Command: "vllm"},
		},
		Capabilities: &AppCapabilities{
			WireEndpoints: []string{"chat_completions", "completions", "embeddings", "responses"},
		},
	}
}

// TestAppsConfig_SaveToDir round-trips a config through the per-provider
// directory writer and re-loads it.
func TestAppsConfig_SaveToDir(t *testing.T) {
	cfg := providersFixture(map[string]Provider{
		"ollama": &ExternalProvider{
			Name:     "ollama",
			Enabled:  new(true),
			Protocol: ProtocolOllama,
			Runtime: ExternalRuntime{
				Endpoint: "http://localhost:11434",
				API:      &APIConfig{AuthType: "bearer"},
			},
			Capabilities: &AppCapabilities{
				WireEndpoints: []string{"chat_completions", "completions", "embeddings"},
			},
		},
	})
	cfg.Version = "1.0"
	cfg.Name = "test"

	dir := t.TempDir()
	if err := cfg.SaveToDir(dir); err != nil {
		t.Fatalf("SaveToDir() error = %v", err)
	}

	// File at <dir>/external/ollama/config.yaml should exist with banner.
	want := filepath.Join(dir, "external", "ollama", "config.yaml")
	body, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("expected %s to exist: %v", want, err)
	}
	if !strings.HasPrefix(string(body), "# MANAGED FILE") {
		t.Errorf("missing MANAGED FILE banner; got first line %q", firstLine(body))
	}

	loaded, err := LoadAppsConfig(dir)
	if err != nil {
		t.Fatalf("Failed to reload saved config: %v", err)
	}
	if loaded.Find("ollama") == nil {
		t.Error("reloaded config missing ollama")
	}
}

func TestAppsConfig_SaveAppParameters(t *testing.T) {
	cfg := providersFixture(map[string]Provider{"vllm": vllmTestProvider()})

	params := map[string]string{
		"gpu-memory": "8GB",
		"max-tokens": "4096",
	}
	env := map[string]string{"CUDA_VISIBLE_DEVICES": "0"}

	if err := cfg.SaveAppParameters("vllm", params, env, nil); err != nil {
		t.Fatalf("SaveAppParameters() error = %v", err)
	}

	vllmConfig, ok := cfg.LookupApp("vllm")
	if !ok || vllmConfig.Defaults == nil {
		t.Fatal("Defaults should not be nil")
	}
	if vllmConfig.Defaults.Parameters["gpu-memory"] != "8GB" {
		t.Errorf("gpu-memory = %q, want %q", vllmConfig.Defaults.Parameters["gpu-memory"], "8GB")
	}
	if vllmConfig.Defaults.Environment["CUDA_VISIBLE_DEVICES"] != "0" {
		t.Errorf("CUDA_VISIBLE_DEVICES = %q, want %q", vllmConfig.Defaults.Environment["CUDA_VISIBLE_DEVICES"], "0")
	}
}

func TestAppsConfig_SaveAppParameters_AppNotFound(t *testing.T) {
	cfg := providersFixture(map[string]Provider{})

	if err := cfg.SaveAppParameters("nonexistent", nil, nil, nil); err == nil {
		t.Error("SaveAppParameters should return error for non-existent app")
	}
}

func TestAppsConfig_SaveAppParameters_EmptyValues(t *testing.T) {
	cfg := providersFixture(map[string]Provider{"vllm": vllmTestProvider()})

	params := map[string]string{
		"gpu-memory": "",     // Empty - should be skipped
		"max-tokens": "4096", // Non-empty - should be saved
	}

	if err := cfg.SaveAppParameters("vllm", params, nil, nil); err != nil {
		t.Fatalf("SaveAppParameters() error = %v", err)
	}

	vllmConfig, _ := cfg.LookupApp("vllm")
	if vllmConfig.Defaults == nil {
		t.Fatal("Defaults should not be nil")
	}
	if _, exists := vllmConfig.Defaults.Parameters["gpu-memory"]; exists {
		t.Error("Empty gpu-memory should not be saved")
	}
	if vllmConfig.Defaults.Parameters["max-tokens"] != "4096" {
		t.Errorf("max-tokens should be saved")
	}
}

func TestAppsConfig_SaveModelDefaults_EmptyApp(t *testing.T) {
	cfg := providersFixture(map[string]Provider{})

	if err := cfg.SaveModelDefaults("", "model", nil, nil); err == nil {
		t.Error("SaveModelDefaults should return error for empty app")
	}
}

func TestAppsConfig_SaveModelDefaults_EmptyModel(t *testing.T) {
	cfg := providersFixture(map[string]Provider{"vllm": vllmTestProvider()})

	if err := cfg.SaveModelDefaults("vllm", "", nil, nil); err == nil {
		t.Error("SaveModelDefaults should return error for empty model name")
	}
}

func TestAppsConfig_SaveModelDefaults_NilApps(t *testing.T) {
	cfg := &AppsConfig{}

	if err := cfg.SaveModelDefaults("vllm", "model", nil, nil); err == nil {
		t.Error("SaveModelDefaults should return error for nil providers")
	}
}

func TestAppsConfig_SaveModelDefaults_NewModel(t *testing.T) {
	cfg := providersFixture(map[string]Provider{"vllm": vllmTestProvider()})

	params := map[string]string{
		"temperature": "0.7",
		"top-p":       "0.9",
	}

	if err := cfg.SaveModelDefaults("vllm", "llama2:7b", params, nil); err != nil {
		t.Fatalf("SaveModelDefaults() error = %v", err)
	}

	vllmConfig, _ := cfg.LookupApp("vllm")
	if vllmConfig.Models == nil {
		t.Fatal("Models should not be nil")
	}
	modelConfig, exists := vllmConfig.Models["llama2:7b"]
	if !exists {
		t.Fatal("Model 'llama2:7b' should exist in Models")
	}
	if modelConfig.Parameters["temperature"] != "0.7" {
		t.Errorf("temperature = %q, want %q", modelConfig.Parameters["temperature"], "0.7")
	}
}

// TestAppsConfig_Resolve_FourTier exercises the full precedence chain:
// defaults < model < node < node-model. Each tier overrides the previous.
func TestAppsConfig_Resolve_FourTier(t *testing.T) {
	sc := ServiceConfig{
		Defaults: &AppDefaultsConfig{
			Parameters: map[string]string{
				"dtype":         "bf16",
				"max-model-len": "1024",
				"tp":            "1",
				"gpu-util":      "0.85",
			},
		},
		Models: map[string]ModelSpec{
			"llama-3-70b": {Parameters: map[string]string{"dtype": "fp16", "max-model-len": "8192"}},
		},
		Nodes: map[string]NodeSpec{
			"worker-1": {
				Parameters: map[string]string{"tp": "4", "gpu-util": "0.92"},
				Models: map[string]NodeModelSpec{
					"llama-3-70b": {Parameters: map[string]string{"max-model-len": "32768"}},
				},
			},
		},
	}

	got := sc.Resolve("worker-1", "llama-3-70b")
	cases := []struct {
		key       string
		wantValue string
		wantTier  Tier
	}{
		{"dtype", "fp16", TierModel},
		{"tp", "4", TierNode},
		{"gpu-util", "0.92", TierNode},
		{"max-model-len", "32768", TierNodeModel},
	}
	for _, tc := range cases {
		rv, ok := got.Parameters[tc.key]
		if !ok {
			t.Errorf("Resolve missing key %q", tc.key)
			continue
		}
		if rv.Value != tc.wantValue {
			t.Errorf("Resolve[%s].Value = %v, want %q", tc.key, rv.Value, tc.wantValue)
		}
		if rv.Tier != tc.wantTier {
			t.Errorf("Resolve[%s].Tier = %v (%s), want %v (%s)", tc.key, rv.Tier, rv.Tier, tc.wantTier, tc.wantTier)
		}
	}
}

// TestAppsConfig_NodeModelCRUD round-trips a Tier 3 write + delete.
func TestAppsConfig_NodeModelCRUD(t *testing.T) {
	cfg := providersFixture(map[string]Provider{"vllm": vllmTestProvider()})

	if err := cfg.SetNodeModelParameter("vllm", "worker-1", "llama-3-70b", "max-model-len", "32768"); err != nil {
		t.Fatalf("SetNodeModelParameter: %v", err)
	}
	sc, _ := cfg.LookupApp("vllm")
	nm := sc.Nodes["worker-1"].Models["llama-3-70b"]
	if nm.Parameters["max-model-len"] != "32768" {
		t.Errorf("NodeModel write lost: %+v", nm)
	}

	if err := cfg.DeleteNodeModelParameter("vllm", "worker-1", "llama-3-70b", "max-model-len"); err != nil {
		t.Fatalf("DeleteNodeModelParameter: %v", err)
	}
	sc, _ = cfg.LookupApp("vllm")
	if len(sc.Nodes) != 0 {
		t.Errorf("empty tree should collapse to nil, got %+v", sc.Nodes)
	}
}

// TestAppsConfig_ThreeMeaningRule verifies plan §4.3: empty string survives
// write→read round-trip as present-with-empty, absent stays absent. `null`
// never appears on disk — our maps are map[string]string, so yaml.v3 cannot
// emit null for a value we set.
func TestAppsConfig_ThreeMeaningRule(t *testing.T) {
	cfg := providersFixture(map[string]Provider{"vllm": vllmTestProvider()})

	if err := cfg.SetAppEnvironment("vllm", "EMPTY_VAR", ""); err != nil {
		t.Fatalf("SetAppEnvironment: %v", err)
	}
	if err := cfg.SetAppEnvironment("vllm", "SET_VAR", "value"); err != nil {
		t.Fatalf("SetAppEnvironment: %v", err)
	}

	dir := t.TempDir()
	if err := cfg.SaveToDir(dir); err != nil {
		t.Fatalf("SaveToDir: %v", err)
	}
	reloaded, err := LoadAppsConfig(dir)
	if err != nil {
		t.Fatalf("LoadAppsConfig: %v", err)
	}

	sc, ok := reloaded.LookupApp("vllm")
	if !ok {
		t.Fatal("vllm not found after reload")
	}
	env := sc.Defaults.Environment
	if v, present := env["EMPTY_VAR"]; !present || v != "" {
		t.Errorf("EMPTY_VAR should be present with empty string; got present=%v v=%q", present, v)
	}
	if v, present := env["SET_VAR"]; !present || v != "value" {
		t.Errorf("SET_VAR should be present with %q; got present=%v v=%q", "value", present, v)
	}
	if _, present := env["ABSENT_VAR"]; present {
		t.Error("ABSENT_VAR should not materialize as empty")
	}
}

// ============================================================================
// YAML Helper Tests
// ============================================================================

func TestIsYAMLStructure(t *testing.T) {
	tests := []struct {
		value    string
		expected bool
	}{
		{"[1, 2, 3]", true},
		{"{key: value}", true},
		{"123", true},
		{"45.67", true},
		{"9GB", false},
		{"16MB", false},
		{"hello", false},
		{"true", false}, // Not a structure
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got := isYAMLStructure(tt.value)
			if got != tt.expected {
				t.Errorf("isYAMLStructure(%q) = %v, want %v", tt.value, got, tt.expected)
			}
		})
	}
}

func TestEnsureParameterQuotes(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "add quotes to unquoted value",
			input:    "  key: value",
			expected: "  key: \"value\"",
		},
		{
			name:     "preserve already quoted",
			input:    "  key: \"value\"",
			expected: "  key: \"value\"",
		},
		{
			name:     "preserve single quoted",
			input:    "  key: 'value'",
			expected: "  key: 'value'",
		},
		{
			name:     "preserve arrays",
			input:    "  key: [1, 2, 3]",
			expected: "  key: [1, 2, 3]",
		},
		{
			name:     "preserve objects",
			input:    "  key: {a: b}",
			expected: "  key: {a: b}",
		},
		{
			name:     "preserve booleans",
			input:    "  key: true",
			expected: "  key: true",
		},
		{
			name:     "preserve null",
			input:    "  key: null",
			expected: "  key: null",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(ensureParameterQuotes([]byte(tt.input)))
			if got != tt.expected {
				t.Errorf("ensureParameterQuotes(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestFixWindowsPathsInYAML(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		contains string
	}{
		{
			name:     "unquoted windows path",
			input:    "  path: C:\\Users\\test\\file.yaml",
			contains: "\\\\", // Escaped backslashes
		},
		{
			name:     "quoted windows path",
			input:    "  path: \"C:\\Users\\test\\file.yaml\"",
			contains: "\\\\", // Escaped backslashes
		},
		{
			name:     "non-windows path",
			input:    "  path: /home/user/file.yaml",
			contains: "/home/user", // No change needed
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(FixWindowsPathsInYAML([]byte(tt.input)))
			if !containsString(got, tt.contains) {
				t.Errorf("FixWindowsPathsInYAML(%q) = %q, should contain %q", tt.input, got, tt.contains)
			}
		})
	}
}

func TestAppProtocolConstants(t *testing.T) {
	tests := []struct {
		protocol AppProtocol
		expected string
	}{
		{ProtocolOllama, "ollama"},
		{ProtocolOpenAI, "openai"},
	}

	for _, tt := range tests {
		if string(tt.protocol) != tt.expected {
			t.Errorf("AppProtocol %v = %q, want %q", tt.protocol, string(tt.protocol), tt.expected)
		}
	}
}

// ============================================================================
// ServiceConfig Tests
// ============================================================================

func TestServiceConfig_IsEnabled(t *testing.T) {
	tests := []struct {
		name     string
		enabled  *bool
		expected bool
	}{
		// nil = auto-discovery mode, IsEnabled() returns false (not explicitly enabled)
		// Use IsAutoDiscovery() to check if app is in auto-discovery mode
		{"nil (auto-discovery)", nil, false},
		{"true", new(true), true},
		{"false", new(false), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ServiceConfig{Enabled: tt.enabled}
			got := cfg.IsEnabled()
			if got != tt.expected {
				t.Errorf("IsEnabled() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestServiceConfig_IsExplicitlyDisabled(t *testing.T) {
	tests := []struct {
		name     string
		enabled  *bool
		expected bool
	}{
		{"nil (auto-discovery)", nil, false},
		{"true", new(true), false},
		{"false", new(false), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ServiceConfig{Enabled: tt.enabled}
			got := cfg.IsExplicitlyDisabled()
			if got != tt.expected {
				t.Errorf("IsExplicitlyDisabled() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// ============================================================================
// Helper Functions
// ============================================================================

func containsString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// An entry holding only an endpoint overlay is not empty: cleanup must
// keep it, or a write setting just an overlay is dropped and clearing a
// tier's last plain parameter takes its overlays with it.
func TestCleanupKeepsEndpointOnlySpecs(t *testing.T) {
	overlay := map[string]EndpointOverlay{"embeddings": {Parameters: map[string]string{"pooling": "mean"}}}
	sc := &ServiceConfig{
		Models: map[string]ModelSpec{"m": {Endpoints: overlay}},
		Nodes: map[string]NodeSpec{
			"n1": {Endpoints: overlay},
			"n2": {Models: map[string]NodeModelSpec{"m": {Endpoints: overlay}}},
		},
	}
	CleanupModelSpecFor(sc, "m")
	CleanupNodeSpecFor(sc, "n1")
	CleanupNodeModelSpecFor(sc, "n2", "m")

	for site, got := range map[string]map[string]EndpointOverlay{
		"models.m":          sc.Models["m"].Endpoints,
		"nodes.n1":          sc.Nodes["n1"].Endpoints,
		"nodes.n2.models.m": sc.Nodes["n2"].Models["m"].Endpoints,
	} {
		if !reflect.DeepEqual(got, overlay) {
			t.Errorf("%s: overlay dropped, got %v", site, got)
		}
	}
}
