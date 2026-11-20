package prov_apps

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/protocol"
)

// OperationTarget identifies one run on its owning node, without an address override.
type OperationTarget struct {
	Provider string `json:"provider"`
	Node     string `json:"node"`
	RunID    string `json:"run_id"`
	Model    string `json:"model"`
	Runtime  string `json:"runtime"`
}

// OperationPreconditions binds execution to the run snapshot returned by discovery.
type OperationPreconditions struct {
	TargetRevision string `json:"target_revision"`
}

// OperationRequest carries the discovered definition and exact target preconditions.
type OperationRequest struct {
	Version            int                    `json:"version"`
	DefinitionRevision string                 `json:"definition_revision"`
	Target             OperationTarget        `json:"target"`
	Preconditions      OperationPreconditions `json:"preconditions"`
	Input              any                    `json:"input"`
}

// OperationResult records the definition and target actually used.
type OperationResult struct {
	OperationID        string          `json:"operation_id"`
	Version            int             `json:"version"`
	DefinitionRevision string          `json:"definition_revision"`
	Target             OperationTarget `json:"target"`
	TargetRevision     string          `json:"target_revision"`
	ResultKind         string          `json:"result_kind"`
	Result             any             `json:"result"`
}

// OperationError is a closed diagnostic refusal suitable for the existing Problem envelope.
type OperationError struct {
	Status int
	Code   string
	Detail string
}

func (e *OperationError) Error() string { return e.Detail }

func operationError(status int, code, detail string) error {
	return &OperationError{Status: status, Code: code, Detail: detail}
}

// ExecuteOperation validates the current definition and leases exactly one live local run.
func (m *ProviderAppManager) ExecuteOperation(ctx context.Context, id string, request OperationRequest, client *http.Client) (*OperationResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cfg := m.AppsConfig()
	operation, ok := cfg.Operation(id)
	if !ok {
		return nil, operationError(http.StatusNotFound, "operation_not_found", "Operation is not defined on this node")
	}
	definition := operation.Definition()
	if request.Version != definition.Version || request.DefinitionRevision != operation.Revision() {
		return nil, operationError(http.StatusConflict, "operation_definition_stale", "Discover the current operation definition before executing")
	}
	if definition.Adapter != "run_json_read" || definition.Audience != "admin" {
		return nil, operationError(http.StatusForbidden, "operation_forbidden", "Operation requires an administrator diagnostic adapter")
	}
	if request.Target.Node != m.localNodename() || request.Target.Provider != operation.Provider() {
		return nil, operationError(http.StatusConflict, "operation_target_mismatch", "Operation target does not identify this provider and node")
	}
	if err := operation.ValidateInput(request.Input); err != nil {
		return nil, operationError(http.StatusBadRequest, "operation_input_invalid", "Input does not satisfy the operation schema")
	}
	run, info, err := m.operationRun(operation, request)
	if err != nil {
		return nil, err
	}
	lease, err := run.Acquire(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	current, found := m.AppsConfig().Operation(id)
	if !found || current.Revision() != operation.Revision() {
		return nil, operationError(http.StatusConflict, "operation_definition_stale", "The operation definition changed during admission")
	}
	currentRun, _, err := m.operationRun(operation, request)
	if err != nil {
		return nil, err
	}
	if currentRun != run {
		return nil, operationError(http.StatusConflict, "operation_target_stale", "The run was replaced during admission")
	}
	// A run owns its local port. Advertised BaseURL can name an interface the engine never bound.
	target := protocol.NewTarget(protocol.ProcessRunEndpoint(run.Port), backend.Engine()).Ready()
	runCtx := lease.Context()
	result, err := target.ReadJSON(runCtx, client, definition.Binding.Method, definition.Binding.Path, request.Input)
	if err != nil {
		if runCtx.Err() != nil {
			return nil, runCtx.Err()
		}
		return nil, operationError(http.StatusBadGateway, "operation_upstream_invalid", fmt.Sprintf("Diagnostic failed: %s", err))
	}
	if err := operation.ValidateOutput(result); err != nil {
		return nil, operationError(http.StatusBadGateway, "operation_output_invalid", "Upstream JSON does not satisfy the operation output schema")
	}
	return &OperationResult{OperationID: id, Version: definition.Version, DefinitionRevision: operation.Revision(), Target: request.Target, TargetRevision: info.TargetRevision, ResultKind: "json", Result: result}, nil
}

// operationRun never resolves a model name: discovery selected this exact registry identity.
func (m *ProviderAppManager) operationRun(operation *config.CompiledOperation, request OperationRequest) (*instance.Instance, instance.InstanceInfo, error) {
	run, ok := m.instances.Get(request.Target.RunID)
	if !ok || run.GetStatus() != instance.StatusRunning || run.Context().Err() != nil {
		return nil, instance.InstanceInfo{}, operationError(http.StatusConflict, "operation_target_unavailable", "The exact run is no longer running; discover a current target")
	}
	info := run.ToInfo(m.localNodename())
	if info.Provider != request.Target.Provider || info.Model != request.Target.Model || info.Runtime != request.Target.Runtime || !operation.AppliesToRuntime(info.Runtime) {
		return nil, instance.InstanceInfo{}, operationError(http.StatusConflict, "operation_target_mismatch", "Run identity or runtime does not match the operation target")
	}
	if request.Preconditions.TargetRevision == "" || request.Preconditions.TargetRevision != info.TargetRevision {
		return nil, instance.InstanceInfo{}, operationError(http.StatusConflict, "operation_target_stale", "Discover the current run revision before executing")
	}
	_, svc, found := m.resolveConfigKey(info.Provider)
	enabled := svc.EnabledOn(m.localNodename())
	if !found || enabled == nil || !*enabled || Lifecycle(svc) != LifecycleProcess || run.Port < 1 || run.Port > 65535 {
		return nil, instance.InstanceInfo{}, operationError(http.StatusConflict, "operation_target_unavailable", "The provider no longer owns an enabled local process for this run")
	}
	return run, info, nil
}
