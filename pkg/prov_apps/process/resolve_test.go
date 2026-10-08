package process

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/model/integrity"
)

// TestResolveModelToPath_GGUFRepoDirectory pins the auto_deploy regression:
// llama.cpp repos download to <root>/<repo>/<file>.gguf, but a bare repo
// path used to resolve to the directory itself, which llama-server can't
// load. Resolution must narrow to the .gguf file inside.
func TestResolveModelToPath_GGUFRepoDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ZZROUTER_MODEL_DIR", root)

	repoDir := filepath.Join(root, "Qwen", "Qwen2.5-0.5B-Instruct-GGUF")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	gguf := filepath.Join(repoDir, "qwen2.5-0.5b-instruct-q4_k_m.gguf")
	if err := os.WriteFile(gguf, []byte("GGUF"), 0o644); err != nil {
		t.Fatalf("write gguf: %v", err)
	}

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"bare-repo-single-gguf", "Qwen/Qwen2.5-0.5B-Instruct-GGUF", gguf},
		{"hash-exact-filename", "Qwen/Qwen2.5-0.5B-Instruct-GGUF#qwen2.5-0.5b-instruct-q4_k_m.gguf", gguf},
		{"hash-pattern-substring", "Qwen/Qwen2.5-0.5B-Instruct-GGUF#Q4_K_M", gguf},
		{"unmatched-hint-falls-back-to-the-only-gguf", "Qwen/Qwen2.5-0.5B-Instruct-GGUF#Q8_0", gguf},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, ok := resolveModelToPath(tc.input)
			if !ok || got != tc.want {
				t.Errorf("resolveModelToPath(%q) = %q, %v; want %q, true", tc.input, got, ok, tc.want)
			}
		})
	}
}

// TestResolveModelToPath_MultipleGGUFsRequireHint confirms that when a repo
// holds several .gguf files, the resolver does not arbitrarily pick one —
// the directory is returned (caller error) unless a hint disambiguates.
func TestResolveModelToPath_MultipleGGUFsRequireHint(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ZZROUTER_MODEL_DIR", root)

	repoDir := filepath.Join(root, "vendor", "multi")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	q4 := filepath.Join(repoDir, "model-Q4_K_M.gguf")
	q8 := filepath.Join(repoDir, "model-Q8_0.gguf")
	for _, p := range []string{q4, q8} {
		if err := os.WriteFile(p, []byte("GGUF"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	if got, ok := resolveModelToPath("vendor/multi"); !ok || got != repoDir {
		t.Errorf("ambiguous repo should return dir, got %q, %v", got, ok)
	}
	if got, ok := resolveModelToPath("vendor/multi#Q4_K_M"); !ok || got != q4 {
		t.Errorf("hinted Q4_K_M should narrow to %q, got %q, %v", q4, got, ok)
	}
	if got, ok := resolveModelToPath("vendor/multi#Q8_0"); !ok || got != q8 {
		t.Errorf("hinted Q8_0 should narrow to %q, got %q, %v", q8, got, ok)
	}
}

// TestResolveModelToPath_NonGGUFDirPassesThrough confirms vllm/MLX-style
// safetensors directories are still returned as directories — the
// dir-to-file narrowing only fires when .gguf files are present.
func TestResolveModelToPath_NonGGUFDirPassesThrough(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ZZROUTER_MODEL_DIR", root)

	repoDir := filepath.Join(root, "Qwen", "Qwen2.5-0.5B")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "model.safetensors"), []byte("st"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got, ok := resolveModelToPath("Qwen/Qwen2.5-0.5B"); !ok || got != repoDir {
		t.Errorf("safetensors repo dir should pass through, got %q, %v", got, ok)
	}
}

// TestResolveModelToPath_ReflectsDownloadsThatLandLater guards the
// staleness the resolved-path cache used to introduce: a launch attempted
// before the weights finished downloading must not pin "not there" — or a
// bare directory — for the rest of the process's life.
func TestResolveModelToPath_ReflectsDownloadsThatLandLater(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ZZROUTER_MODEL_DIR", root)

	if _, ok := resolveModelToPath("vendor/late"); ok {
		t.Fatal("model should not resolve before it is downloaded")
	}

	repoDir := filepath.Join(root, "vendor", "late")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	gguf := filepath.Join(repoDir, "late-Q4_K_M.gguf")
	if err := os.WriteFile(gguf, []byte("GGUF"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, ok := resolveModelToPath("vendor/late")
	if !ok || got != gguf {
		t.Errorf("after download, want %q, true; got %q, %v", gguf, got, ok)
	}
}

// A feature's file beside the weights is never taken for them, whatever
// the hint, and a split model in a quant subdirectory resolves to its first
// shard.
func TestResolveModelToPath_FeatureFilesAndShards(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ZZROUTER_MODEL_DIR", root)
	repo := filepath.Join(root, "unsloth", "M-GGUF")
	write := func(rel string) string {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("GGUF"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	weights := write("model-F16.gguf") // sorts after mmproj-F16.gguf, so only the exclusion keeps it out
	write("mmproj-F16.gguf")
	first := write("Q8_0/M-Q8_0-00001-of-00002.gguf")
	write("Q8_0/M-Q8_0-00002-of-00002.gguf")
	if err := integrity.RecordFiles(repo, "unsloth/M-GGUF", "", integrity.FileChecksum{RelativePath: "mmproj-F16.gguf", Size: 4, Feature: "vision"}); err != nil {
		t.Fatal(err)
	}

	for input, want := range map[string]string{
		"unsloth/M-GGUF#F16":                             weights,
		"unsloth/M-GGUF#Q8_0":                            first,
		"unsloth/M-GGUF#M-Q8_0-00002-of-00002.gguf":      repo, // an exact path is the full relative path
		"unsloth/M-GGUF#Q8_0/M-Q8_0-00002-of-00002.gguf": first,
	} {
		if got, ok := resolveModelToPath(input); !ok || got != want {
			t.Errorf("resolveModelToPath(%q) = %q; want %q", input, got, want)
		}
	}
}

func TestResolveDeclaredProjectorGlobs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ZZROUTER_MODEL_DIR", root)
	dir := filepath.Join(root, "vendor/legacy")
	require.NoError(t, os.MkdirAll(dir, 0700))
	for _, name := range []string{"a-projector.gguf", "z-weights.gguf"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0600))
	}
	for _, input := range []string{"vendor/legacy", dir, "vendor/legacy#a-projector.gguf"} {
		got, ok := resolveModelToPath(input, "a-projector.gguf")
		require.True(t, ok)
		assert.Equal(t, filepath.Join(dir, "z-weights.gguf"), got)
	}
	_, ok := resolveModelToPath(filepath.Join(dir, "a-projector.gguf"), "a-projector.gguf")
	assert.False(t, ok, "a projector path must never be accepted as weights")
}
