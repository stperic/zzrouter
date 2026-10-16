package httperr

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRouteErrorCodeWireValuesAreClosedAndStable(t *testing.T) {
	require.Equal(t, map[string]string{
		"CodeModelUnpriced":             "model_unpriced",
		"CodeModelInUse":                "model_in_use",
		"CodeModelReferenced":           "model_referenced",
		"CodeModelNotDeletable":         "model_not_deletable",
		"CodeCapacityExceeded":          "capacity_exceeded",
		"CodeQueueWaitExpired":          "queue_wait_expired",
		"CodeReleaseManifestMissing":    "release_manifest_missing",
		"CodeProtocolCutoverRequired":   "protocol_cutover_required",
		"CodeJobNotFound":               "job_not_found",
		"CodeModelNodeAmbiguous":        "model_node_ambiguous",
		"CodeModelSourceAmbiguous":      "model_source_ambiguous",
		"CodeModelDetailUnsupported":    "model_detail_unsupported",
		"CodeNodeUnreachable":           "node_unreachable",
		"CodeClusterUnavailable":        "cluster_unavailable",
		"CodeUpstreamTimeout":           "upstream_timeout",
		"CodeUpstreamCanceled":          "upstream_canceled",
		"CodeUpstreamShape":             "upstream_shape",
		"CodeUpstreamStreamInterrupted": "upstream_stream_interrupted",
	}, declaredErrorCodes(t, "route_code.go", "RouteErrorCode"))
}
