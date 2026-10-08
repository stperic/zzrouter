package update

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/version"
)

// smokeTestTimeout bounds the version probe. Printing a version is the
// cheapest thing the binary does; anything that takes longer than this
// is hung, not slow.
const smokeTestTimeout = 30 * time.Second

// ErrSmokeTestFailed reports that a downloaded binary could not be run.
// The install stops before touching anything, so the node keeps the
// version it is already running.
var ErrSmokeTestFailed = errors.New("the downloaded binary failed its smoke test")

// smokeTest runs the extracted binary and checks it reports the version
// the release promised.
//
// Checksums prove the bytes arrived intact; they say nothing about
// whether those bytes run here. A release built for another
// architecture, against a newer libc, or from a broken build has a
// perfectly good checksum and still cannot start. Since the update now
// exits the process to hand it to the supervisor, "cannot start" means
// the node stays down, so this runs before anything on disk is
// replaced.
//
// This execs a binary from the internet, which is only safe because
// applyUpdate verifies the archive against the signed checksums first;
// keep this call after verification.
func smokeTest(ctx context.Context, binaryPath string, expected *version.Version) error {
	ctx, cancel := context.WithTimeout(ctx, smokeTestTimeout)
	defer cancel()

	output, err := host.CommandContext(ctx, binaryPath, "--version").CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("%w: it did not report a version within %s", ErrSmokeTestFailed, smokeTestTimeout)
	}
	if err != nil {
		return fmt.Errorf("%w: %w (%s)", ErrSmokeTestFailed, err, firstLine(output))
	}

	reported := strings.TrimSpace(string(output))
	if reported == "" {
		return fmt.Errorf("%w: it reported no version at all", ErrSmokeTestFailed)
	}
	if expected != nil && !strings.Contains(reported, expected.String()) {
		return fmt.Errorf("%w: it reports %q, but the release is %s",
			ErrSmokeTestFailed, firstLine(output), expected.String())
	}

	return nil
}

// firstLine keeps an error message to one line of a binary's output.
func firstLine(output []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(output)), "\n")
	if len(line) > 200 {
		return line[:200] + "…"
	}
	return line
}
