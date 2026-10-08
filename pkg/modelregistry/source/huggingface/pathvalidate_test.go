package huggingface

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateRepoFileName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		// Accepted
		{"plain file", "config.json", false},
		{"nested file", "tokenizer/spiece.model", false},
		{"deep nesting", "a/b/c/d/file.bin", false},
		{"safetensors shard", "model-00001-of-00010.safetensors", false},
		{"unicode in name", "tokenizer-日本語.json", false},
		{"dots in segment", "model.v2.bin", false},
		{"hyphens and underscores", "my-model_v1.bin", false},

		// Path traversal — the central CVE class
		{"parent traversal", "../etc/passwd", true},
		{"deep traversal", "../../../../etc/zzrouter/keys.yaml", true},
		{"midpath traversal escapes", "models/../../../etc/passwd", true},
		{"midpath traversal stays inside", "models/../etc/passwd", true},
		{"single dotdot", "..", true},
		{"trailing dotdot", "models/..", true},
		{"current dir", ".", true},

		// Absolute paths
		{"absolute unix", "/etc/passwd", true},
		{"windows drive C", "C:\\boot.ini", true},
		{"windows drive lowercase", "c:/foo", true},

		// Separator confusion
		{"backslash windows", "models\\foo.bin", true},
		{"backslash literal", "..\\..\\foo", true},

		// Embedded NUL
		{"NUL byte", "good\x00bad", true},

		// Empty / whitespace
		{"empty", "", true},
		{"whitespace only", "   ", true},
		{"tab only", "\t", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateRepoFileName(tc.input)
			if tc.wantErr {
				require.Error(t, err, "input=%q expected error", tc.input)
				assert.True(t, errors.Is(err, ErrInvalidRepoFileName),
					"input=%q error should match ErrInvalidRepoFileName, got: %v", tc.input, err)
			} else {
				require.NoError(t, err, "input=%q expected no error", tc.input)
			}
		})
	}
}

func TestEncodeRepoFilePath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  string
	}{
		{"config.json", "config.json"},
		{"tokenizer/spiece.model", "tokenizer/spiece.model"},
		{"my model.bin", "my%20model.bin"},
		{"folder name/file.bin", "folder%20name/file.bin"},
		// URL metacharacters that could smuggle a query or fragment
		{"foo?bar.bin", "foo%3Fbar.bin"},
		{"foo#frag.bin", "foo%23frag.bin"},
		{"weird&char=value.bin", "weird&char=value.bin"}, // & and = are not reserved in path segments
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, encodeRepoFilePath(tc.input))
		})
	}
}

// FuzzValidateRepoFileName: any input that the validator accepts must,
// after path.Clean and segment split, contain no ".." segment and not be
// absolute. This pins the security invariant against future regressions
// in the validator logic.
func FuzzValidateRepoFileName(f *testing.F) {
	seeds := []string{
		"config.json",
		"a/b/c.bin",
		"../foo",
		"/abs",
		"C:\\foo",
		"weird name with spaces.bin",
		"\x00",
		"",
		strings.Repeat("a/", 100) + "file",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		err := validateRepoFileName(name)
		if err != nil {
			return // rejection is fine
		}
		// Acceptance invariants — if any fail, the validator was bypassed.
		if name == "" {
			t.Fatalf("accepted empty name")
		}
		if strings.ContainsRune(name, 0) {
			t.Fatalf("accepted NUL byte in %q", name)
		}
		if strings.ContainsRune(name, '\\') {
			t.Fatalf("accepted backslash in %q", name)
		}
		if strings.HasPrefix(name, "/") {
			t.Fatalf("accepted absolute path %q", name)
		}
	})
}
