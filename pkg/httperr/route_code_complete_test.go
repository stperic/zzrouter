package httperr

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRouteErrorCodeWireValuesAreClosedAndStable(t *testing.T) {
	require.Equal(t, map[string]string{
		"CodeReleaseManifestMissing":    "release_manifest_missing",
		"CodeProtocolCutoverRequired":   "protocol_cutover_required",
		"CodeJobNotFound":               "job_not_found",
		"CodeModelNodeAmbiguous":        "model_node_ambiguous",
		"CodeNodeUnreachable":           "node_unreachable",
		"CodeClusterUnavailable":        "cluster_unavailable",
		"CodeUpstreamTimeout":           "upstream_timeout",
		"CodeUpstreamCanceled":          "upstream_canceled",
		"CodeUpstreamShape":             "upstream_shape",
		"CodeUpstreamStreamInterrupted": "upstream_stream_interrupted",
	}, declaredErrorCodes(t, "route_code.go", "RouteErrorCode"))
}
