package gpu

import (
	"context"
)

// ProbeContext returns the current Detection for the given vendor on
// this node. It never returns an error for "no GPU" — that is a valid
// answer (StateAbsent). An error is returned only when the platform
// itself cannot be inspected (no supported probe implementation).
// Cancelling the context aborts any in-flight shell-out and returns
// whatever partial detection was gathered.
//
// If the context was wrapped by WithCachedInventory, the Detection
// is served from the cache — same nvidia-smi invocation that seeded
// the cache, no additional subprocess call.
func ProbeContext(ctx context.Context, vendor Vendor) (Detection, error) {
	if det, ok := detectionFromCache(ctx, vendor); ok {
		return det, nil
	}
	return probePlatform(ctx, vendor)
}
