//go:build linux

package servercli

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/host"
)

// argv builds a NUL-separated /proc/<pid>/cmdline, trailing NUL and all,
// which is the exact shape the kernel hands back.
func argv(words ...string) []byte {
	return append([]byte(strings.Join(words, "\x00")), 0)
}

// TestArgvIsServer pins the discriminator that keeps the privileged
// updater from being mistaken for a second server.
//
// Without it the restarted node sees the updater -- same binary, same
// executable name -- refuses to start, and restart-loops until the
// updater gives up and rolls back a release that was never at fault.
// Observed on a real systemd box, not in a unit test, which is why the
// decision now lives in a function a unit test can reach.
func TestArgvIsServer(t *testing.T) {
	node := "/opt/zzrouter/versions/1.0.0/bin/zzrouter-node"

	tests := []struct {
		name string
		argv []byte
		want bool
	}{
		{"the node serving", argv(node, "start", "--no-setup"), true},
		{"the privileged updater", argv(node, "update", "run"), false},
		{"an operator reading status", argv(node, "update", "status"), false},
		{"the relocation command", argv(node, "update", "enable"), false},
		// argv[0] is the path and is never the answer: on a managed
		// install the updater's argv[0] is this same binary.
		{"a path that merely contains the word", argv("/opt/start/zzrouter-node", "version"), false},
		// What a process that has exited but not been reaped looks like.
		{"a zombie", []byte{}, false},
		{"a zombie with only the trailing NUL", []byte{0}, false},
		{"no arguments at all", argv(node), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, argvIsServer(tt.argv))
		})
	}
}

// TestIsServerProcess checks the /proc read on a live process.
func TestIsServerProcess(t *testing.T) {
	notServing := host.Command("sleep", "60")
	require.NoError(t, notServing.Start())
	t.Cleanup(func() {
		_ = notServing.Process.Kill()
		_, _ = notServing.Process.Wait()
	})

	// This is the shape that matters: a live process running our binary
	// on some subcommand that is not the server.
	assert.False(t, isServerProcess(notServing.Process.Pid))

	// A pid that cannot be read has to fall the safe way: refusing to
	// start beats two servers sharing one host's ports and state.
	assert.True(t, isServerProcess(-1), "an unreadable process must be assumed to be a server")

	// The test binary itself runs no server subcommand.
	assert.False(t, isServerProcess(os.Getpid()))
}
