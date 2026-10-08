package gpu

import (
	"context"
	"errors"
	"os/exec"

	"github.com/stperic/zzrouter/pkg/host"
)

// queryRocmSMI runs `rocm-smi <args>` and returns the raw stdout.
// rocm-smi has wildly different output formats across flags (key/value
// blocks for --showmeminfo, percentages for --showuse, plain lists
// for --showid, no CSV mode for most), so a structured-row helper
// like queryNVIDIASMI doesn't fit. Instead this is the lowest useful
// shared layer: subprocess + missing-binary semantics + ctx, leaving
// each caller to parse its own output format.
//
// Returns (nil, nil) when rocm-smi is absent from PATH — "no driver"
// is a valid answer for callers collecting optional metrics, and
// forcing every caller to branch on exec.ErrNotFound would just
// duplicate the same check. Any other error (permission, early exit)
// is returned so callers can log it.
//
// ctx bounds the subprocess; a cancelled ctx aborts rocm-smi and
// the helper returns ctx.Err() to the caller.
func queryRocmSMI(ctx context.Context, args ...string) ([]byte, error) {
	out, err := host.CommandContext(ctx, "rocm-smi", args...).Output()
	if err != nil {
		// rocm-smi missing is not an error — it just means no AMD
		// ROCm stack is installed on this node. errors.Is unwraps
		// *exec.Error under Go 1.21+ so one check suffices.
		if errors.Is(err, exec.ErrNotFound) {
			return nil, nil
		}
		// Fold rocm-smi's stderr into the returned error so
		// operators see the real diagnostic ("HSA_STATUS_ERROR:
		// Runtime not available" and friends) instead of a bare
		// exit code.
		return nil, wrapWithStderr(err, "rocm-smi")
	}
	return out, nil
}
