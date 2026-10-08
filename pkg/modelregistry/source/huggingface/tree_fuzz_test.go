package huggingface

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// FuzzParseRepoFilesJSON pins the security invariant that no entry
// returned by the parser will fail validateRepoFileName. The HF JSON
// shape is the entry point for repo-controlled data; if a malformed or
// adversarial response can sneak a "../etc/passwd" filename through,
// the path-traversal CVE is back open.
//
// This fuzz target also catches panics in the generic-map decoding
// path (any-typed unmarshaling has historically been a footgun for
// nil-deref crashes when type assertions land on unexpected shapes).
func FuzzParseRepoFilesJSON(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"siblings":[{"rfilename":"config.json","size":100}]}`),
		[]byte(`{"siblings":[]}`),
		[]byte(`{}`),
		[]byte(`null`),
		[]byte(`[]`),
		// The shape any future poisoned-repo CVE will look like:
		[]byte(`{"siblings":[{"rfilename":"../../../etc/passwd","size":0}]}`),
		[]byte(`{"siblings":[{"rfilename":"/etc/shadow"}]}`),
		[]byte(`{"siblings":[{"rfilename":"good.bin"},{"rfilename":"../bad"},{"rfilename":"more.bin"}]}`),
		// Junk shapes the generic-map decoder must survive without panicking
		[]byte(`{"siblings":["string instead of object"]}`),
		[]byte(`{"siblings":[null]}`),
		[]byte(`{"siblings":[{"rfilename":12345}]}`),
		[]byte(`{"siblings":[{"rfilename":"x","size":"not a number"}]}`),
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		files, err := parseRepoFilesJSON(body, "fuzz/model")
		if err != nil {
			return // unparseable JSON is a fine outcome
		}
		// Acceptance invariants: every returned filename must pass the
		// validator. If any does not, the gate has been bypassed.
		for _, f := range files {
			require.NoError(t, validateRepoFileName(f.Name),
				"parser returned filename that fails validation: %q", f.Name)
		}
	})
}
