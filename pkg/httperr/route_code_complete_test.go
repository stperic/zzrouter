package httperr

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRouteErrorCodeWireValuesAreClosedAndStable(t *testing.T) {
	require.Equal(t, map[string]string{
		"CodeCatalogPublicationFailed":      "catalog_publication_failed",
		"CodeTemplateNotFound":              "template_not_found",
		"CodeTemplateAmbiguous":             "template_ambiguous",
		"CodeTemplateUnsupported":           "template_unsupported",
		"CodeTemplateSourceUnavailable":     "template_source_unavailable",
		"CodeWorkflowModelMetadataMissing":  "workflow_model_metadata_missing",
		"CodeModelUnpriced":                 "model_unpriced",
		"CodeModelInUse":                    "model_in_use",
		"CodeModelReferenced":               "model_referenced",
		"CodeModelNotDeletable":             "model_not_deletable",
		"CodeCapacityExceeded":              "capacity_exceeded",
		"CodeQueueWaitExpired":              "queue_wait_expired",
		"CodeReleaseManifestMissing":        "release_manifest_missing",
		"CodeProtocolCutoverRequired":       "protocol_cutover_required",
		"CodeJobNotFound":                   "job_not_found",
		"CodeModelNodeAmbiguous":            "model_node_ambiguous",
		"CodeRunNodeRequired":               "run_node_required",
		"CodeRunNotFound":                   "run_not_found",
		"CodeContextTooSmall":               "context_too_small",
		"CodeConcurrencyAboveLimit":         "concurrency_above_limit",
		"CodeRunNotReady":                   "run_not_ready",
		"CodeRunChanged":                    "run_changed",
		"CodeCalibrationUnsupported":        "calibration_unsupported",
		"CodeCalibrationRunning":            "calibration_running",
		"CodeCalibrationWrongNode":          "calibration_wrong_node",
		"CodeRunSettingsDiffer":             "run_settings_differ",
		"CodeModelSourceAmbiguous":          "model_source_ambiguous",
		"CodeModelDetailUnsupported":        "model_detail_unsupported",
		"CodeNodeUnreachable":               "node_unreachable",
		"CodeClusterUnavailable":            "cluster_unavailable",
		"CodeUpstreamTimeout":               "upstream_timeout",
		"CodeUpstreamCanceled":              "upstream_canceled",
		"CodeUpstreamShape":                 "upstream_shape",
		"CodeUpstreamStreamInterrupted":     "upstream_stream_interrupted",
		"CodeProviderSyncPending":           "provider_sync_pending",
		"CodePinIsClusterWide":              "pin_is_cluster_wide",
		"CodeProviderConnectionUnavailable": "provider_connection_unavailable",
		"CodeModelDiscoveryUnavailable":     "model_discovery_unavailable",
		"CodeAccessStoreFailed":             "access_store_failed",
		"CodeAccessStoreBusy":               "access_store_busy",
		"CodeStateBackupNotFound":           "state_backup_not_found",
		"CodeStateBackupCapacityExceeded":   "state_backup_capacity_exceeded",
		"CodeStateImageCheckFailed":         "state_image_check_failed",
		"CodeStateRevisionChanged":          "state_revision_changed",
		"CodeStateBackupUnprotected":        "state_backup_unprotected",
	}, declaredErrorCodes(t, "route_code.go", "RouteErrorCode"))
}
