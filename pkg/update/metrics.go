package update

import (
	"context"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// UpdateMetrics holds all update-related metrics instruments.
type UpdateMetrics struct {
	// Check metrics
	updateChecks        metric.Int64Counter
	updateCheckDuration metric.Float64Histogram

	// Download metrics
	updateDownloads        metric.Int64Counter
	updateDownloadDuration metric.Float64Histogram
	updateDownloadBytes    metric.Int64Counter

	// Install metrics
	updateInstalls        metric.Int64Counter
	updateInstallDuration metric.Float64Histogram

	// Rollback metrics
	updateRollbacks metric.Int64Counter

	// State metrics
	updatePending metric.Int64UpDownCounter
}

var (
	globalUpdateMetrics *UpdateMetrics
	updateMetricsOnce   sync.Once
	updateMetricsErr    error
)

// InitUpdateMetrics initializes the update metrics. Thread-safe via sync.Once.
func InitUpdateMetrics() (*UpdateMetrics, error) {
	updateMetricsOnce.Do(func() {
		globalUpdateMetrics, updateMetricsErr = newUpdateMetrics()
	})
	return globalUpdateMetrics, updateMetricsErr
}

// GetUpdateMetrics returns the global update metrics instance.
// Returns nil if metrics haven't been initialized or failed to initialize.
func GetUpdateMetrics() *UpdateMetrics {
	if globalUpdateMetrics == nil {
		_, _ = InitUpdateMetrics()
	}
	return globalUpdateMetrics
}

func newUpdateMetrics() (*UpdateMetrics, error) {
	meter := otel.Meter("zzrouter.update")
	m := &UpdateMetrics{}
	var err error

	// Check metrics
	m.updateChecks, err = meter.Int64Counter(
		"zzrouter.update.checks",
		metric.WithDescription("Total update check attempts"),
		metric.WithUnit("{check}"),
	)
	if err != nil {
		return nil, err
	}

	m.updateCheckDuration, err = meter.Float64Histogram(
		"zzrouter.update.check_duration",
		metric.WithDescription("Update check duration in seconds"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.1, 0.5, 1, 2, 5, 10, 30, 60),
	)
	if err != nil {
		return nil, err
	}

	// Download metrics
	m.updateDownloads, err = meter.Int64Counter(
		"zzrouter.update.downloads",
		metric.WithDescription("Total update download attempts"),
		metric.WithUnit("{download}"),
	)
	if err != nil {
		return nil, err
	}

	m.updateDownloadDuration, err = meter.Float64Histogram(
		"zzrouter.update.download_duration",
		metric.WithDescription("Update download duration in seconds"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 30, 60, 120, 300, 600),
	)
	if err != nil {
		return nil, err
	}

	m.updateDownloadBytes, err = meter.Int64Counter(
		"zzrouter.update.download_bytes",
		metric.WithDescription("Total bytes downloaded for updates"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	// Install metrics
	m.updateInstalls, err = meter.Int64Counter(
		"zzrouter.update.installs",
		metric.WithDescription("Total update install attempts"),
		metric.WithUnit("{install}"),
	)
	if err != nil {
		return nil, err
	}

	m.updateInstallDuration, err = meter.Float64Histogram(
		"zzrouter.update.install_duration",
		metric.WithDescription("Update install duration in seconds"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.1, 0.5, 1, 2, 5, 10, 30),
	)
	if err != nil {
		return nil, err
	}

	// Rollback metrics
	m.updateRollbacks, err = meter.Int64Counter(
		"zzrouter.update.rollbacks",
		metric.WithDescription("Total rollback attempts"),
		metric.WithUnit("{rollback}"),
	)
	if err != nil {
		return nil, err
	}

	// State metrics
	m.updatePending, err = meter.Int64UpDownCounter(
		"zzrouter.update.pending",
		metric.WithDescription("Number of pending updates"),
		metric.WithUnit("{update}"),
	)
	if err != nil {
		return nil, err
	}

	return m, nil
}

// Update status constants for consistency.
const (
	UpdateStatusSuccess     = "success"
	UpdateStatusFailed      = "failed"
	UpdateStatusSkipped     = "skipped"
	UpdateStatusCancelled   = "cancelled"
	UpdateStatusRateLimited = "rate_limited"
)

// RecordUpdateCheck records an update check operation.
func (m *UpdateMetrics) RecordUpdateCheck(ctx context.Context, status string, durationSeconds float64, updateAvailable bool) {
	if m == nil {
		return
	}

	attrs := []attribute.KeyValue{
		attribute.String("status", status),
		attribute.Bool("update_available", updateAvailable),
	}

	m.updateChecks.Add(ctx, 1, metric.WithAttributes(attrs...))
	m.updateCheckDuration.Record(ctx, durationSeconds, metric.WithAttributes(attrs...))
}

// RecordUpdateDownload records an update download operation.
func (m *UpdateMetrics) RecordUpdateDownload(ctx context.Context, status string, durationSeconds float64, bytes int64, resumed bool) {
	if m == nil {
		return
	}

	attrs := []attribute.KeyValue{
		attribute.String("status", status),
		attribute.Bool("resumed", resumed),
	}

	m.updateDownloads.Add(ctx, 1, metric.WithAttributes(attrs...))
	m.updateDownloadDuration.Record(ctx, durationSeconds, metric.WithAttributes(attrs...))

	if bytes > 0 {
		m.updateDownloadBytes.Add(ctx, bytes, metric.WithAttributes(
			attribute.Bool("resumed", resumed),
		))
	}
}

// RecordUpdateInstall records an update install operation.
func (m *UpdateMetrics) RecordUpdateInstall(ctx context.Context, status, fromVersion, toVersion string, durationSeconds float64) {
	if m == nil {
		return
	}

	attrs := []attribute.KeyValue{
		attribute.String("status", status),
		attribute.String("from_version", fromVersion),
		attribute.String("to_version", toVersion),
	}

	m.updateInstalls.Add(ctx, 1, metric.WithAttributes(attrs...))
	m.updateInstallDuration.Record(ctx, durationSeconds, metric.WithAttributes(attrs...))
}

// RecordUpdateRollback records a rollback operation.
func (m *UpdateMetrics) RecordUpdateRollback(ctx context.Context, status string) {
	if m == nil {
		return
	}

	m.updateRollbacks.Add(ctx, 1, metric.WithAttributes(
		attribute.String("status", status),
	))
}

// SetPendingUpdate sets the pending update state.
func (m *UpdateMetrics) SetPendingUpdate(ctx context.Context, pending bool) {
	if m == nil {
		return
	}

	if pending {
		m.updatePending.Add(ctx, 1)
	} else {
		m.updatePending.Add(ctx, -1)
	}
}

// UpdateRecorder is a helper for recording update operation metrics with automatic timing.
type UpdateRecorder struct {
	ctx       context.Context
	metrics   *UpdateMetrics
	operation string
	startTime time.Time
}

// NewUpdateCheckRecorder creates a recorder for update check operations.
func NewUpdateCheckRecorder(ctx context.Context) *UpdateRecorder {
	return &UpdateRecorder{
		ctx:       ctx,
		metrics:   GetUpdateMetrics(),
		operation: "check",
		startTime: utils.Now(),
	}
}

// RecordCheckResult records the result of an update check.
func (r *UpdateRecorder) RecordCheckResult(status string, updateAvailable bool) {
	if r == nil || r.metrics == nil {
		return
	}
	duration := time.Since(r.startTime).Seconds()
	r.metrics.RecordUpdateCheck(r.ctx, status, duration, updateAvailable)
}

// NewUpdateDownloadRecorder creates a recorder for download operations.
func NewUpdateDownloadRecorder(ctx context.Context) *UpdateRecorder {
	return &UpdateRecorder{
		ctx:       ctx,
		metrics:   GetUpdateMetrics(),
		operation: "download",
		startTime: utils.Now(),
	}
}

// RecordDownloadResult records the result of a download operation.
func (r *UpdateRecorder) RecordDownloadResult(status string, bytes int64, resumed bool) {
	if r == nil || r.metrics == nil {
		return
	}
	duration := time.Since(r.startTime).Seconds()
	r.metrics.RecordUpdateDownload(r.ctx, status, duration, bytes, resumed)
}

// NewUpdateInstallRecorder creates a recorder for install operations.
func NewUpdateInstallRecorder(ctx context.Context) *UpdateRecorder {
	return &UpdateRecorder{
		ctx:       ctx,
		metrics:   GetUpdateMetrics(),
		operation: "install",
		startTime: utils.Now(),
	}
}

// RecordInstallResult records the result of an install operation.
func (r *UpdateRecorder) RecordInstallResult(status, fromVersion, toVersion string) {
	if r == nil || r.metrics == nil {
		return
	}
	duration := time.Since(r.startTime).Seconds()
	r.metrics.RecordUpdateInstall(r.ctx, status, fromVersion, toVersion, duration)
}
