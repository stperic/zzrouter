package pythonvenv

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/security"
	"github.com/stretchr/testify/require"
)

func TestManagedPythonCommandPrioritizesPipDiagnostics(t *testing.T) {
	python, err := exec.LookPath("python3")
	require.NoError(t, err)
	noise := strings.Repeat("Collecting dependency\n", 100)
	diagnostics := "ERROR: Could not find a version that satisfies the requirement numpy<1.0,>=1.25.0\nERROR: No matching distribution found for numpy<1.0,>=1.25.0"
	secret := "hf_InventedCredentialForRedaction1234" //nolint:gosec // Synthetic credential exercises diagnostic redaction.
	// Real command failure, synthetic pip output; no package install or network.
	script := "import sys; sys.stdout.write(sys.argv[1]); sys.stdout.flush(); sys.stderr.write(sys.argv[2]); sys.exit(7)"
	output := noise + diagnostics + "\ntoken=" + secret + "\n"
	err = runPython(t.Context(), python, "", nil, "-c", script, noise, diagnostics+"\ntoken="+secret+"\n")
	require.Error(t, err)
	require.True(t, strings.HasPrefix(err.Error(), diagnostics+"\n\n"), "%s", err)
	require.Contains(t, err.Error(), security.RedactSensitive(output), "the entire retained output must remain available")
	require.NotContains(t, err.Error(), secret)
	var exit *exec.ExitError
	require.True(t, errors.As(err, &exit))
	require.Equal(t, 7, exit.ExitCode())
}

func TestManagedPythonCommandWithoutPipDiagnostics(t *testing.T) {
	python, err := exec.LookPath("python3")
	require.NoError(t, err)
	err = runPython(t.Context(), python, "", nil, "-c", "import sys; print('Traceback: import failed'); sys.exit(2)")
	require.ErrorContains(t, err, "Traceback: import failed")
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	require.Equal(t, 2, exit.ExitCode())
	require.NoError(t, runPython(t.Context(), python, "", nil, "-c", "print('ERROR: output alone does not fail a successful command')"))
}
