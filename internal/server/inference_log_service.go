// package server provides HTTP handlers for the zzrouter host server.
// InferenceLogService - Business logic layer for inference log operations

package server

import (
	"time"

	"github.com/stperic/zzrouter/pkg/inferencelog"
	"github.com/stperic/zzrouter/pkg/utils"
)

// InferenceLogService provides business logic for querying the inference log store.
type InferenceLogService struct {
	store *inferencelog.Store
}

// NewInferenceLogService creates a new inference log service.
func NewInferenceLogService(store *inferencelog.Store) *InferenceLogService {
	return &InferenceLogService{store: store}
}

// Query returns log entries matching the filter.
func (s *InferenceLogService) Query(filter inferencelog.QueryFilter) []inferencelog.LogEntry {
	return s.store.Query(filter)
}

// Get returns a single log entry by ID.
func (s *InferenceLogService) Get(id string) (inferencelog.LogEntry, bool) {
	return s.store.Get(id)
}

// Payload returns the retained request bodies for an entry.
func (s *InferenceLogService) Payload(id string) (inferencelog.Payload, bool) {
	return s.store.Payload(id)
}

// Len returns the number of entries in the store.
func (s *InferenceLogService) Len() int {
	return s.store.Len()
}

// ModelUsageReport is the response body for the model usage endpoints.
//
// Since is when accumulation began, which is node start. The window is
// deliberately not configurable: the totals are running counters, so there is
// no earlier point they could be reported from.
type ModelUsageReport struct {
	Since       string                    `json:"since"`
	UptimeSecs  float64                   `json:"uptime_seconds"`
	Models      []inferencelog.ModelUsage `json:"models,omitempty"`
	ModelDetail *inferencelog.ModelUsage  `json:"model,omitempty"`
}

// ModelUsage returns running per-model usage since node start. Pass an empty
// model for every model; pass an empty provider to sum a model name across
// the providers serving it.
func (s *InferenceLogService) ModelUsage(provider, model string) (*ModelUsageReport, bool) {
	totals := s.store.Totals()
	since := totals.Since()

	report := &ModelUsageReport{
		Since:      since.UTC().Format(time.RFC3339),
		UptimeSecs: utils.Now().Sub(since).Seconds(),
	}

	if model == "" {
		report.Models = totals.All()
		return report, true
	}

	usage, ok := totals.ForModel(provider, model)
	if !ok {
		return nil, false
	}
	report.ModelDetail = &usage
	return report, true
}
