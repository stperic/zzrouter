package download

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLookupChecksum(t *testing.T) {
	doc := `# comment line ignored
abc123  ollama-linux-amd64.tar.zst
def456 *ollama-windows-amd64.zip
malformed-only-one-field
xyz789  llama-b6000-bin-win-cuda-13.1-x64.zip
`
	cases := []struct {
		archive string
		want    string
	}{
		{"ollama-linux-amd64.tar.zst", "abc123"},
		{"ollama-windows-amd64.zip", "def456"}, // tolerates "*" prefix
		{"llama-b6000-bin-win-cuda-13.1-x64.zip", "xyz789"},
		{"missing.zip", ""},
	}
	for _, tc := range cases {
		got := lookupChecksum(doc, tc.archive)
		if got != tc.want {
			t.Errorf("lookup(%q) = %q, want %q", tc.archive, got, tc.want)
		}
	}
}

// Upstream publishes every entry as "./name". Matched literally that never
// hit, so the lookup returned "" and the caller logged "not listed, skipping
// verification" for every ollama artifact ever shipped — the whole reason
// these archives come from GitHub rather than ollama.com.
func TestLookupChecksumMatchesPublishedNameForms(t *testing.T) {
	// Verbatim shape of v0.32.14/sha256sum.txt.
	const doc = `5ae5bca5f0d297f5e35665e01db399a69a8eac3f8fad89cd9d2531fd495c9457  ./ollama-windows-amd64.zip
c620917a00000000000000000000000000000000000000000000000000000000  ./ollama-linux-amd64.tar.zst
7802b739fbdc74df556600f1619f86457b69dce913301cf2d91f7f9d7f7a41b8  ./ollama-linux-arm64.tar.zst`

	assert.Equal(t, "c620917a00000000000000000000000000000000000000000000000000000000",
		lookupChecksum(doc, "ollama-linux-amd64.tar.zst"))

	// The other two forms upstreams use must keep working.
	assert.Equal(t, "abc123", lookupChecksum("abc123  plain.tgz", "plain.tgz"))
	assert.Equal(t, "def456", lookupChecksum("def456 *binmode.zip", "binmode.zip"))

	// An artifact genuinely absent still reports absent.
	assert.Empty(t, lookupChecksum(doc, "ollama-darwin.tgz"))
}
