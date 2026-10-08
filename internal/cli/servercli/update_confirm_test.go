package servercli

import (
	"testing"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
)

// TestNodeProbeAddresses: the admin listener binds node.bind, which may
// be a single interface. A self-probe that only tried loopback would
// never connect on such a node and would roll back every healthy
// release.
//
// Covers both halves of a verified update, which is the point of the
// probe being one type: the node confirming the version it booted on,
// and the privileged updater confirming the version it restarted the
// node onto, must not be able to reach different answers.
func TestNodeProbeAddresses(t *testing.T) {
	probeFor := func(bind string) nodeProbe {
		cfg := &pkgConfig.NodeConfig{}
		cfg.Node.Bind = bind
		cfg.Node.Port = 9090
		return nodeProbe{cfg: cfg}
	}

	for _, wildcard := range []string{"", "0.0.0.0", "::", "[::]"} {
		assert.Equal(t, []string{"127.0.0.1", "localhost"}, probeFor(wildcard).addresses(),
			"a wildcard bind is reachable on loopback: %q", wildcard)
	}

	// A pinned address is tried first, and loopback is still tried after
	// it: a worker's listener is forced to loopback whatever node.bind
	// says.
	assert.Equal(t, []string{"192.0.2.10", "127.0.0.1", "localhost"}, probeFor("192.0.2.10").addresses())
}
