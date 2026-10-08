package update

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInitUpdateMetrics(t *testing.T) {
	metrics, err := InitUpdateMetrics()
	assert.NoError(t, err)
	assert.NotNil(t, metrics)
}

func TestGetUpdateMetrics(t *testing.T) {
	metrics := GetUpdateMetrics()
	assert.NotNil(t, metrics)
}

func TestUpdateMetrics_RecordUpdateCheck(t *testing.T) {
	metrics := GetUpdateMetrics()
	assert.NotNil(t, metrics)

	ctx := context.Background()
	// Should not panic
	metrics.RecordUpdateCheck(ctx, UpdateStatusSuccess, 1.5, true)
	metrics.RecordUpdateCheck(ctx, UpdateStatusFailed, 0.5, false)
	metrics.RecordUpdateCheck(ctx, UpdateStatusRateLimited, 0.1, false)
}

func TestUpdateMetrics_RecordUpdateDownload(t *testing.T) {
	metrics := GetUpdateMetrics()
	assert.NotNil(t, metrics)

	ctx := context.Background()
	// Should not panic
	metrics.RecordUpdateDownload(ctx, UpdateStatusSuccess, 60.0, 1024*1024, false)
	metrics.RecordUpdateDownload(ctx, UpdateStatusSuccess, 30.0, 512*1024, true)
	metrics.RecordUpdateDownload(ctx, UpdateStatusFailed, 5.0, 0, false)
}

func TestUpdateMetrics_RecordUpdateInstall(t *testing.T) {
	metrics := GetUpdateMetrics()
	assert.NotNil(t, metrics)

	ctx := context.Background()
	// Should not panic
	metrics.RecordUpdateInstall(ctx, UpdateStatusSuccess, "1.0.0", "1.1.0", 2.5)
	metrics.RecordUpdateInstall(ctx, UpdateStatusFailed, "1.1.0", "1.2.0", 1.0)
}

func TestUpdateMetrics_RecordUpdateRollback(t *testing.T) {
	metrics := GetUpdateMetrics()
	assert.NotNil(t, metrics)

	ctx := context.Background()
	// Should not panic
	metrics.RecordUpdateRollback(ctx, UpdateStatusSuccess)
	metrics.RecordUpdateRollback(ctx, UpdateStatusFailed)
}

func TestUpdateMetrics_SetPendingUpdate(t *testing.T) {
	metrics := GetUpdateMetrics()
	assert.NotNil(t, metrics)

	ctx := context.Background()
	// Should not panic
	metrics.SetPendingUpdate(ctx, true)
	metrics.SetPendingUpdate(ctx, false)
}

func TestUpdateMetrics_NilReceiver(t *testing.T) {
	var metrics *UpdateMetrics
	ctx := context.Background()

	// None of these should panic with nil receiver
	metrics.RecordUpdateCheck(ctx, UpdateStatusSuccess, 1.0, false)
	metrics.RecordUpdateDownload(ctx, UpdateStatusSuccess, 1.0, 1024, false)
	metrics.RecordUpdateInstall(ctx, UpdateStatusSuccess, "1.0.0", "1.1.0", 1.0)
	metrics.RecordUpdateRollback(ctx, UpdateStatusSuccess)
	metrics.SetPendingUpdate(ctx, true)
}

func TestUpdateRecorder_CheckResult(t *testing.T) {
	ctx := context.Background()
	recorder := NewUpdateCheckRecorder(ctx)
	assert.NotNil(t, recorder)

	// Should not panic
	recorder.RecordCheckResult(UpdateStatusSuccess, true)
}

func TestUpdateRecorder_DownloadResult(t *testing.T) {
	ctx := context.Background()
	recorder := NewUpdateDownloadRecorder(ctx)
	assert.NotNil(t, recorder)

	// Should not panic
	recorder.RecordDownloadResult(UpdateStatusSuccess, 1024*1024, false)
}

func TestUpdateRecorder_InstallResult(t *testing.T) {
	ctx := context.Background()
	recorder := NewUpdateInstallRecorder(ctx)
	assert.NotNil(t, recorder)

	// Should not panic
	recorder.RecordInstallResult(UpdateStatusSuccess, "1.0.0", "1.1.0")
}

func TestUpdateRecorder_NilReceiver(t *testing.T) {
	var recorder *UpdateRecorder

	// None of these should panic with nil receiver
	recorder.RecordCheckResult(UpdateStatusSuccess, false)
	recorder.RecordDownloadResult(UpdateStatusSuccess, 0, false)
	recorder.RecordInstallResult(UpdateStatusSuccess, "", "")
}

func TestUpdateStatusConstants(t *testing.T) {
	// Verify constants are defined correctly
	assert.Equal(t, "success", UpdateStatusSuccess)
	assert.Equal(t, "failed", UpdateStatusFailed)
	assert.Equal(t, "skipped", UpdateStatusSkipped)
	assert.Equal(t, "cancelled", UpdateStatusCancelled)
	assert.Equal(t, "rate_limited", UpdateStatusRateLimited)
}
