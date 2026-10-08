package update

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stretchr/testify/require"
)

func TestReleaseTagClassification(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("Bash is required only for the release script test")
	}
	for _, tc := range []struct {
		ref, prerelease, patch string
		valid                  bool
	}{
		{"refs/tags/v1.2.3", "false", "3", true},
		{"refs/tags/v1.2.3-lab.20261006", "true", "3", true},
		{"refs/tags/v1.2.3-rc.1+build.42", "true", "3", true},
		{"refs/tags/v1.2.3+build.42", "false", "3", true},
		{"refs/heads/main", "", "", false},
		{"refs/tags/v1.2.3;exit", "", "", false},
		{"refs/tags/v1.2.3-lab..1", "", "", false},
		{"refs/tags/v1.2.3-01", "", "", false},
		{"refs/tags/v1.2.3+build..1", "", "", false},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "output")
			cmd := host.Command("bash", "../../.github/scripts/release-version.sh")
			cmd.Env = []string{"GITHUB_REF=" + tc.ref, "GITHUB_OUTPUT=" + output}
			_, err := cmd.CombinedOutput()
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			b, err := os.ReadFile(output)
			require.NoError(t, err)
			require.Contains(t, string(b), "is_prerelease="+tc.prerelease+"\n")
			require.Contains(t, string(b), "patch="+tc.patch+"\n")
		})
	}
}

func TestReleaseWorkflowUsesTagSigning(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release.yaml")
	require.NoError(t, err)
	s := string(b)
	require.Equal(t, 2, strings.Count(s, "run: bash .github/scripts/release-version.sh"))
	require.Contains(t, s, "prerelease: ${{ steps.version.outputs.is_prerelease }}")
	require.Contains(t, s, "id-token: write")
	require.Contains(t, s, "--bundle checksums.txt.sigstore.json")
	require.Contains(t, s, "/.github/workflows/release.yaml@${{ github.ref }}")
	require.NotContains(t, s, "workflow_dispatch:")
}
