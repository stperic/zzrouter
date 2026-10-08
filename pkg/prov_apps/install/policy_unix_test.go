//go:build !windows

package install

import (
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProtectedAncestorAccessDenial(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root intentionally cannot authorize overrides")
	}
	path := "/etc/hosts"
	if runtime.GOOS == "darwin" {
		path = "/private/etc/hosts"
	}
	// Inspect only metadata of a standard protected file; never modify the host.
	_, err := checkPolicyPath(path)
	require.NoError(t, err)
}
