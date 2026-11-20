package resources

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/stperic/zzrouter/pkg/host"
)

// The aggregate ROCm view is a bridge until AMD has a per-device sampler.

// QueryRocmSMI preserves raw ROCm formats and treats a missing binary as absent.
func QueryRocmSMI(ctx context.Context, args ...string) ([]byte, error) {
	out, err := host.CommandContext(ctx, "rocm-smi", args...).Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, nil
		}
		return nil, DriverError(err, "rocm-smi")
	}
	return out, nil
}

// DriverError retains the vendor tool's diagnostic alongside its exit error.
func DriverError(err error, binary string) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(exit.Stderr) > 0 {
		return fmt.Errorf("%s: %w: %s", binary, err, strings.TrimSpace(string(exit.Stderr)))
	}
	return err
}
