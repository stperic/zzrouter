package prov_apps

import (
	"fmt"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

type runStartFailure struct {
	run   *instance.Instance
	cause error
}

func (e *runStartFailure) Error() string { return e.cause.Error() }
func (e *runStartFailure) Unwrap() error { return e.cause }

// CatalogPublicationError preserves a completed mutation's publication failure.
type CatalogPublicationError struct {
	RunID  string
	JobID  string
	Status instance.Status
	Cause  error
}

func (e *CatalogPublicationError) Error() string {
	return fmt.Sprintf("run %s is %s; catalog publication failed: %v; inspect this run with zz_runs before retrying", e.RunID, e.Status, e.Cause)
}

// Unwrap preserves transport and coordinator refusal causes.
func (e *CatalogPublicationError) Unwrap() error { return e.Cause }

func publicationFailureMeta(run *instance.Instance) jobs.Meta {
	return jobs.Meta{"phase": "catalog-publication-failed", "catalog_publication": "failed", "code": string(httperr.CodeCatalogPublicationFailed), "run_id": run.ID, "run_status": run.GetStatus()}
}

func (m *ProviderAppManager) publishRunCatalog(run *instance.Instance) error {
	if m.publishCatalog == nil || !run.ChangesCatalog() {
		return nil
	}
	select {
	case <-m.shutdownCtx.Done():
		return nil
	default:
	}
	if err := m.publishCatalog(); err != nil {
		return &CatalogPublicationError{RunID: run.ID, JobID: m.RunJobID(run), Status: run.GetStatus(), Cause: err}
	}
	return nil
}
