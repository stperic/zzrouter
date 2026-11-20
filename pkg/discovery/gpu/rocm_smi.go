package gpu

import (
	"context"

	"github.com/stperic/zzrouter/pkg/discovery/resources"
)

func queryRocmSMI(ctx context.Context, args ...string) ([]byte, error) {
	return resources.QueryRocmSMI(ctx, args...)
}
