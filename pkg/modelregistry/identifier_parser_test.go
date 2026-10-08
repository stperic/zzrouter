package modelregistry

import (
	"testing"

	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
)

// cloudConfigFixture builds an AppsConfig with the named apps registered as
// cloud providers. Used by the Parse cloud-awareness tests.
func cloudConfigFixture(t *testing.T, names ...string) *pkgConfig.AppsConfig {
	t.Helper()
	cfg := &pkgConfig.AppsConfig{Version: "1.0"}
	enabled := true
	for _, n := range names {
		sc := pkgConfig.ServiceConfig{
			Name:    n,
			Mode:    constants.AppModeCloud,
			Enabled: &enabled,
			Runtime: &pkgConfig.AppRuntimeConfig{
				Endpoint:  "https://api.example.com",
				Execution: pkgConfig.ExecutionConfig{Type: "api"},
				API:       &pkgConfig.APIConfig{AuthType: "bearer"},
			},
			Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
		}
		require.NoError(t, cfg.AddApp(n, sc))
	}
	return cfg
}

func TestParse_CloudRegistry_ExplicitHint(t *testing.T) {
	cfg := cloudConfigFixture(t, "openrouter")
	got, err := Parse("anthropic/claude-opus-4.5", ParseOpts{Config: cfg, Registry: "openrouter"})
	require.NoError(t, err)
	require.Equal(t, "openrouter", got.Repository)
	require.Equal(t, "anthropic/claude-opus-4.5", got.ModelName)
	require.Empty(t, got.Tag)
	require.Empty(t, got.Node)
	require.Empty(t, got.File)
}

func TestParse_CloudRegistry_StripsPrefix(t *testing.T) {
	cfg := cloudConfigFixture(t, "openrouter")
	got, err := Parse("openrouter/anthropic/claude", ParseOpts{Config: cfg, Registry: "openrouter"})
	require.NoError(t, err)
	require.Equal(t, "openrouter", got.Repository)
	require.Equal(t, "anthropic/claude", got.ModelName)
}

func TestParse_CloudRegistry_OpaqueSeparators(t *testing.T) {
	cfg := cloudConfigFixture(t, "openrouter")
	got, err := Parse("openai/gpt-4o-mini:2024-07-18", ParseOpts{Config: cfg, Registry: "openrouter"})
	require.NoError(t, err)
	require.Equal(t, "openrouter", got.Repository)
	require.Equal(t, "openai/gpt-4o-mini:2024-07-18", got.ModelName)
	require.Empty(t, got.Tag, "colon must NOT be split as a tag")
}

func TestParse_CloudRegistry_CloudflareAtPrefix(t *testing.T) {
	cfg := cloudConfigFixture(t, "cloudflare")
	got, err := Parse("@cf/meta/llama-3.1-8b-instruct", ParseOpts{Config: cfg, Registry: "cloudflare"})
	require.NoError(t, err)
	require.Equal(t, "cloudflare", got.Repository)
	require.Equal(t, "@cf/meta/llama-3.1-8b-instruct", got.ModelName)
	require.Empty(t, got.Node, "@ must NOT be split as a node")
}

func TestParse_CloudRegistry_AutoDetect(t *testing.T) {
	cfg := cloudConfigFixture(t, "openrouter")
	// No explicit Registry hint; first component matches a configured cloud provider.
	got, err := Parse("openrouter/anthropic/claude", ParseOpts{Config: cfg})
	require.NoError(t, err)
	require.Equal(t, "openrouter", got.Repository)
	require.Equal(t, "anthropic/claude", got.ModelName)
}

func TestParse_LocalRegistry_FullParse(t *testing.T) {
	// "ollama" is NOT registered as cloud; full-parse path must run and
	// extract tag + node per the canonical grammar.
	cfg := cloudConfigFixture(t) // empty
	got, err := Parse("llama3:8b@gpu-1", ParseOpts{Config: cfg, Registry: "ollama"})
	require.NoError(t, err)
	require.Equal(t, "gpu-1", got.Node)
	require.Equal(t, "llama3", got.ModelName)
	require.Equal(t, "8b", got.Tag)
}

func TestParse_NoConfig_FallsThrough(t *testing.T) {
	// Cloud-looking input but opts.Config=nil → cannot short-circuit; full parse runs.
	got, err := Parse("openai/gpt-4o-mini:2024-07-18", ParseOpts{})
	require.NoError(t, err)
	// Full parser treats "openai/gpt-4o-mini" as model (unknown first component)
	// and splits ":2024-07-18" as a tag.
	require.Equal(t, "2024-07-18", got.Tag)
	require.Equal(t, "openai/gpt-4o-mini", got.ModelName)
}

func TestParse_CloudRegistry_HashStaysOpaque(t *testing.T) {
	// Cloud IDs must not be '#'-split either — belt-and-suspenders with the
	// other separator tests.
	cfg := cloudConfigFixture(t, "openrouter")
	got, err := Parse("anthropic/claude-sonnet-4.5#variant", ParseOpts{Config: cfg, Registry: "openrouter"})
	require.NoError(t, err)
	require.Equal(t, "openrouter", got.Repository)
	require.Equal(t, "anthropic/claude-sonnet-4.5#variant", got.ModelName)
	require.Empty(t, got.File, "# must NOT be split as a file suffix for cloud")
}

func TestParse_UnknownRegistryHint_FallsThrough(t *testing.T) {
	// Registry hint names a provider that doesn't exist in config → no
	// cloud short-circuit; the parser falls through to the full parse path.
	cfg := cloudConfigFixture(t, "openrouter") // config only knows openrouter
	got, err := Parse("llama3:8b@gpu-1", ParseOpts{Config: cfg, Registry: "nonexistent"})
	require.NoError(t, err)
	require.Equal(t, "gpu-1", got.Node)
	require.Equal(t, "llama3", got.ModelName)
	require.Equal(t, "8b", got.Tag)
}

func TestParse_Namespace_Extraction(t *testing.T) {
	cfg := cloudConfigFixture(t, "openrouter", "cloudflare")

	cases := []struct {
		name    string
		input   string
		opts    ParseOpts
		wantNS  string
		wantMdl string // ModelName must stay unchanged
	}{
		{
			name:    "huggingface org/model",
			input:   "meta-llama/Llama-3.2-3B-Instruct",
			opts:    ParseOpts{Config: cfg, Registry: "huggingface"},
			wantNS:  "meta-llama",
			wantMdl: "meta-llama/Llama-3.2-3B-Instruct",
		},
		{
			name:    "hf alias also splits",
			input:   "bartowski/Llama-3.2-3B-GGUF",
			opts:    ParseOpts{Config: cfg, Registry: "hf"},
			wantNS:  "bartowski",
			wantMdl: "bartowski/Llama-3.2-3B-GGUF",
		},
		{
			name:    "cloudflare strips @cf/ and takes vendor",
			input:   "@cf/meta/llama-3.1-8b-instruct",
			opts:    ParseOpts{Config: cfg, Registry: "cloudflare"},
			wantNS:  "meta",
			wantMdl: "@cf/meta/llama-3.1-8b-instruct",
		},
		{
			name:    "openrouter provider/model",
			input:   "anthropic/claude-sonnet-4.5",
			opts:    ParseOpts{Config: cfg, Registry: "openrouter"},
			wantNS:  "anthropic",
			wantMdl: "anthropic/claude-sonnet-4.5",
		},
		{
			name:    "ollama bare name — no namespace",
			input:   "llama3:8b",
			opts:    ParseOpts{Config: cfg, Registry: "ollama"},
			wantNS:  "",
			wantMdl: "llama3", // Tag is :8b; ModelName is bare
		},
		{
			name:    "no registry hint — no namespace extraction",
			input:   "meta-llama/Llama-3.2-3B",
			opts:    ParseOpts{Config: cfg}, // no Registry
			wantNS:  "",
			wantMdl: "meta-llama/Llama-3.2-3B",
		},
		{
			name:    "openrouter auto-detect extracts namespace",
			input:   "openrouter/anthropic/claude",
			opts:    ParseOpts{Config: cfg}, // auto-detect from first path component
			wantNS:  "anthropic",
			wantMdl: "anthropic/claude",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.input, tc.opts)
			require.NoError(t, err)
			require.Equal(t, tc.wantNS, got.Namespace, "namespace")
			require.Equal(t, tc.wantMdl, got.ModelName, "ModelName must stay unchanged")
		})
	}
}

func TestParse_LocalFirstComponent_NoShortCircuit(t *testing.T) {
	// Auto-detect must only fire for CLOUD providers. A local registry like
	// 'hf' as the first component must NOT short-circuit — the parser should
	// extract it as Repository and continue splitting model/tag.
	cfg := cloudConfigFixture(t, "openrouter") // openrouter cloud, hf not in fixture
	got, err := Parse("hf/meta-llama/Llama-3:8b", ParseOpts{Config: cfg})
	require.NoError(t, err)
	require.Equal(t, "huggingface", got.Repository, "hf normalizes to huggingface")
	require.Equal(t, "meta-llama/Llama-3", got.ModelName)
	require.Equal(t, "8b", got.Tag, "tag extraction must still happen for local registries")
}

// TestParse_Canonical locks in the canonical <model>@<node> form
// used by the Ollama-compat surface. The rightmost '@' separates model (left)
// from node (right) — matching email / HuggingFace / Docker / Go-module
// conventions where the qualifier follows the artifact.
func TestParse_Canonical(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantNode  string
		wantModel string
		wantTag   string
	}{
		{
			name:      "bare model, no node",
			input:     "llama3:8b",
			wantNode:  "",
			wantModel: "llama3",
			wantTag:   "8b",
		},
		{
			name:      "model at node",
			input:     "llama3:8b@gpu-1",
			wantNode:  "gpu-1",
			wantModel: "llama3",
			wantTag:   "8b",
		},
		{
			name:      "model at node with port",
			input:     "llama3:8b@gpu-1:9090",
			wantNode:  "gpu-1:9090",
			wantModel: "llama3",
			wantTag:   "8b",
		},
		{
			name:      "namespaced model at node",
			input:     "meta-llama/Llama-3-8B@gpu-1",
			wantNode:  "gpu-1",
			wantModel: "meta-llama/Llama-3-8B",
			wantTag:   "",
		},
		{
			name:      "tagged namespaced model at node",
			input:     "meta-llama/Llama-3:70b@gpu-2",
			wantNode:  "gpu-2",
			wantModel: "meta-llama/Llama-3",
			wantTag:   "70b",
		},
		{
			name:      "model with @ in name resolved by rightmost",
			input:     "hf.co/org/model@v1.2@gpu-1",
			wantNode:  "gpu-1",
			wantModel: "hf.co/org/model@v1.2",
			wantTag:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.input, ParseOpts{})
			require.NoError(t, err)
			require.Equal(t, tt.wantNode, got.Node, "Node")
			require.Equal(t, tt.wantModel, got.ModelName, "ModelName")
			require.Equal(t, tt.wantTag, got.Tag, "Tag")
		})
	}
}

// TestParse_NodeFirst verifies the alternate host::model form
// remains supported for internal callers that build node-first identifiers.
func TestParse_NodeFirst(t *testing.T) {
	got, err := Parse("gpu-1::llama3:8b", ParseOpts{})
	require.NoError(t, err)
	require.Equal(t, "gpu-1", got.Node)
	require.Equal(t, "llama3", got.ModelName)
	require.Equal(t, "8b", got.Tag)
}

// TestParse_MixedSeparators rejects inputs that use both forms.
func TestParse_MixedSeparators(t *testing.T) {
	_, err := Parse("gpu-1::llama3@gpu-2", ParseOpts{})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrIdentifierMixedSeparators)
}

// TestParse_Empty returns an empty Identifier with no error.
func TestParse_Empty(t *testing.T) {
	got, err := Parse("", ParseOpts{})
	require.NoError(t, err)
	require.Equal(t, "", got.Node)
	require.Equal(t, "", got.ModelName)
	require.Equal(t, "", got.Tag)
}

// TestParse_FileSuffix locks in the canonical '#file' variant suffix
// extraction. Grammar: <repo-or-model>[:tag][#file][@node]. '#' comes between
// tag and node; the rightmost '#' wins (consistent with '@' handling).
func TestParse_FileSuffix(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantModel string
		wantTag   string
		wantFile  string
		wantNode  string
	}{
		{
			name:      "repo with file suffix",
			input:     "bartowski/Llama-3.2-3B-GGUF#Q4_K_M",
			wantModel: "bartowski/Llama-3.2-3B-GGUF",
			wantFile:  "Q4_K_M",
		},
		{
			name:      "repo with full filename suffix",
			input:     "bartowski/Llama-3.2-3B-GGUF#llama-3.2-3b.Q4_K_M.gguf",
			wantModel: "bartowski/Llama-3.2-3B-GGUF",
			wantFile:  "llama-3.2-3b.Q4_K_M.gguf",
		},
		{
			name:      "no file suffix remains empty",
			input:     "llama3:8b@gpu-1",
			wantModel: "llama3",
			wantTag:   "8b",
			wantNode:  "gpu-1",
		},
		{
			name:      "file and node together",
			input:     "bartowski/Llama#Q4_K_M@gpu-1",
			wantModel: "bartowski/Llama",
			wantFile:  "Q4_K_M",
			wantNode:  "gpu-1",
		},
		{
			name:      "tag, file, and node together",
			input:     "bartowski/Llama:v2#Q4_K_M@gpu-1",
			wantModel: "bartowski/Llama",
			wantTag:   "v2",
			wantFile:  "Q4_K_M",
			wantNode:  "gpu-1",
		},
		{
			name:      "trailing hash, empty file (permissive)",
			input:     "bartowski/Llama#",
			wantModel: "bartowski/Llama",
			wantFile:  "",
		},
		{
			name:      "multiple hashes, last one wins",
			input:     "bartowski/Llama#first#second",
			wantModel: "bartowski/Llama#first",
			wantFile:  "second",
		},
		{
			name:      "clean model, no aux fields",
			input:     "llama3",
			wantModel: "llama3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.input, ParseOpts{})
			require.NoError(t, err)
			require.Equal(t, tt.wantModel, got.ModelName, "ModelName")
			require.Equal(t, tt.wantTag, got.Tag, "Tag")
			require.Equal(t, tt.wantFile, got.File, "File")
			require.Equal(t, tt.wantNode, got.Node, "Node")
		})
	}
}

// TestIdentifier_GetFullModelName_StripsFileAndNode verifies GetFullModelName
// returns just <model>[:tag], stripping both '#file' and '@node' suffixes.
func TestIdentifier_GetFullModelName_StripsFileAndNode(t *testing.T) {
	id, err := Parse("bartowski/Llama:v2#Q4_K_M@gpu-1", ParseOpts{})
	require.NoError(t, err)
	require.Equal(t, "bartowski/Llama:v2", id.GetFullModelName())
}

// TestIdentifier_GetFullModelName reassembles the canonical model portion.
func TestIdentifier_GetFullModelName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"bare model", "llama3", "llama3"},
		{"tagged model", "llama3:8b", "llama3:8b"},
		{"tagged model at node", "llama3:8b@gpu-1", "llama3:8b"},
		{"namespaced tagged model at node", "meta-llama/Llama-3:70b@gpu-2", "meta-llama/Llama-3:70b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := Parse(tt.input, ParseOpts{})
			require.NoError(t, err)
			require.Equal(t, tt.want, id.GetFullModelName())
		})
	}
}
