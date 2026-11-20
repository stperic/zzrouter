package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/calibration"
	"github.com/stperic/zzrouter/pkg/config/runsettings"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/security"
	"github.com/stperic/zzrouter/pkg/utils"
)

type calibrationClaim struct {
	sync.Mutex
	jobID string
	body  [sha256.Size]byte
}

type calibrationRefusal struct {
	code   httperr.RouteErrorCode
	id     string
	detail string
}

func (e calibrationRefusal) Error() string { return e.detail }

func calibrationFailure(code httperr.RouteErrorCode, idField, next string) func(error, *httperr.Error) bool {
	return func(err error, wire *httperr.Error) bool {
		refusal, ok := errors.AsType[calibrationRefusal](err)
		if !ok || refusal.code != code {
			return false
		}
		wire.Message += " " + next
		wire.Extra = map[string]any{idField: refusal.id}
		return true
	}
}

func resolveCalibrationFields(err error, wire *httperr.Error) bool {
	field, ok := errors.AsType[*calibration.FieldError](err)
	if !ok {
		return false
	}
	code := string(httperr.CodeInvalidValue)
	wire.Code = code
	wire.Message = "Correct the fields and retry with zz_calibrate."
	wire.Extra = map[string]any{"errors": []utils.ParamError{{Key: field.Field, Code: code, Message: field.Message}}}
	return true
}

func (s *Server) handleInternalCalibration(c *gin.Context) {
	var req calibration.Request
	if !BindJSONStrict(c, &req) {
		return
	}
	if node := s.GetNodename(); req.Node != node {
		RespondToError(c, newRouteProblemError(http.StatusMisdirectedRequest, http.StatusText(http.StatusMisdirectedRequest),
			fmt.Sprintf("Calibration reached node %q. Check the requested node name with zz_nodes.", node), httperr.CodeCalibrationWrongNode, node))
		return
	}
	result, err := s.startCalibration(c.Request.Context(), req)
	if err != nil {
		RespondToError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, result)
}

func (s *Server) startCalibration(ctx context.Context, req calibration.Request) (calibrationAccepted, error) {
	if err := validateCalibrationRequest(req); err != nil {
		return calibrationAccepted{}, err
	}
	if s.jobs == nil || s.providers.appMgr == nil {
		return calibrationAccepted{}, httperr.Error{Status: http.StatusServiceUnavailable, Message: "Calibration is unavailable. Check the node with zz_nodes."}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return calibrationAccepted{}, err
	}
	key := sha256.Sum256(body)
	claim := &s.calibration
	claim.Lock()
	defer claim.Unlock()
	node := s.GetNodename()
	if claim.jobID != "" {
		if claim.body != key {
			return calibrationAccepted{}, calibrationRefusal{httperr.CodeCalibrationRunning, claim.jobID,
				fmt.Sprintf("Calibration job %q is already running on %q.", claim.jobID, node)}
		}
		event, reuseErr := s.jobs.Reuse(claim.jobID)
		if reuseErr != nil {
			return calibrationAccepted{}, reuseErr
		}
		return calibrationAccepted{JobID: event.JobID, Node: node}, nil
	}

	targets := make(map[string]calibration.Target, len(req.Runs))
	probes := make(map[string]*schema.Observe, len(req.Runs))
	for index, entry := range req.Runs {
		target, observe, err := s.checkCalibrationRun(ctx, node, entry, index)
		if err != nil {
			return calibrationAccepted{}, err
		}
		targets[entry.RunID], probes[entry.RunID] = target, observe
	}
	if err := ctx.Err(); err != nil {
		return calibrationAccepted{}, err
	}
	job, err := s.jobs.Start(s.shutdownCtx, jobs.KindCalibration, "", nil) //nolint:contextcheck // The job follows the node lifetime.
	if err != nil {
		return calibrationAccepted{}, httperr.Error{Status: http.StatusServiceUnavailable, Message: "Calibration jobs are unavailable. Check the node with zz_nodes."}
	}
	claim.jobID, claim.body = job.ID(), key
	nodeRunner := &nodeCalibration{server: s, node: node, targets: targets, probes: probes}
	go nodeRunner.run(job, req, claim) //nolint:contextcheck // The runner is owned by the detached job.
	return calibrationAccepted{JobID: job.ID(), Node: node}, nil
}

func (s *Server) checkCalibrationRun(ctx context.Context, node string, entry calibration.RunRequest, index int) (calibration.Target, *schema.Observe, error) {
	mgr := s.providers.appMgr
	inst, ok := mgr.GetInstance(entry.RunID)
	if !ok {
		return calibration.Target{}, nil, calibrationRefusal{httperr.CodeRunNotFound, entry.RunID, fmt.Sprintf("Run %q is absent on %q.", entry.RunID, node)}
	}
	info := mgr.InstanceInfo(ctx, inst, node)
	if info.Status != instance.StatusRunning {
		return calibration.Target{}, nil, calibrationRefusal{httperr.CodeRunNotReady, entry.RunID, fmt.Sprintf("Run %q is %s.", entry.RunID, info.Status)}
	}
	if entry.TargetRevision != nil && *entry.TargetRevision != info.TargetRevision {
		return calibration.Target{}, nil, calibrationRefusal{httperr.CodeRunChanged, entry.RunID, fmt.Sprintf("Run %q has a different target revision.", entry.RunID)}
	}
	resolved := inst.Resolved()
	if err := validateCalibrationWorkload(entry, index, info, resolved.WireEndpoints); err != nil {
		return calibration.Target{}, nil, err
	}
	cfg, _ := mgr.ProviderConfig(inst.Provider)
	declared := loadProviderSchema(s.configStore, cfg.Name)
	target, observe := calibrationTarget(info, declared)
	return target, observe, nil
}

func validateCalibrationWorkload(entry calibration.RunRequest, index int, info instance.InstanceInfo, endpoints []string) error {
	w := entry.Workload
	if w == nil {
		return nil
	}
	_, required := calibrationEndpoint(info.Endpoint)
	if required == "" || !slices.Contains(endpoints, required) {
		return calibrationRefusal{httperr.CodeCalibrationUnsupported, info.ID, fmt.Sprintf("Run %q has no supported calibration workload API.", info.ID)}
	}
	if err := w.ValidateEndpoint(info.Endpoint); err != nil {
		field, _ := errors.AsType[*calibration.FieldError](err)
		return &calibration.FieldError{Field: fmt.Sprintf("runs[%d].workload.%s", index, field.Field), Message: field.Message}
	}
	if info.Endpoint == calibration.Chat && info.ContextLength > 0 && w.InputTokens > info.ContextLength-*w.OutputTokens {
		return calibrationRefusal{httperr.CodeContextTooSmall, info.ID, fmt.Sprintf("Run %q serves %d context tokens and cannot hold the requested input and output.", info.ID, info.ContextLength)}
	}
	if limit := info.MaxConcurrentRequests; limit > 0 && slices.Max(w.Concurrency) > limit {
		return calibrationRefusal{httperr.CodeConcurrencyAboveLimit, info.ID, fmt.Sprintf("Run %q limits max_concurrent_requests to %d.", info.ID, limit)}
	}
	return nil
}

func calibrationTarget(info instance.InstanceInfo, declared *schema.ProviderSchema) (calibration.Target, *schema.Observe) {
	run := calibration.RunReport{RunID: info.ID, Provider: info.Provider, Model: info.Model, Endpoint: info.Endpoint,
		MaxConcurrentRequests: new(info.MaxConcurrentRequests)}
	run.ResolvedParameters, run.RedactedParameters = security.RedactStringFields(info.ResolvedParameters)
	if status := info.ParametersStatus; status != nil {
		run.ParametersStatus = string(status.State)
		for _, path := range status.Overridden {
			if flag, ok := strings.CutPrefix(path, "parameters."); ok {
				path = runsettings.FlagName(flag)
			}
			run.Overridden = append(run.Overridden, path)
		}
	}
	run.ServedContextTokens = knownPositive(info.ContextLength)
	if cache := info.Cache; cache != nil {
		run.Cache = &calibration.CacheReport{KVTokens: knownPositive(cache.KVTokens), ContextTokens: knownPositive(cache.ContextTokens), MaxConcurrency: knownPositive(cache.MaxConcurrency)}
	}
	observe := declared.Observe
	if !observe.Applies(info.Provider, info.Runtime) {
		observe = nil
	}
	if observe != nil {
		for _, role := range []struct{ name, field string }{
			{"running", observe.Running}, {"waiting", observe.Waiting}, {"kv_cache_usage", observe.KVCacheUsage},
		} {
			if role.field != "" {
				run.ObservedRoles = append(run.ObservedRoles, role.name)
			}
		}
		if len(observe.Busy) > 0 {
			run.ObservedRoles = append(run.ObservedRoles, "busy")
		}
	}
	target := calibration.Target{TargetRevision: info.TargetRevision, Run: run}
	if declared.Calibration != nil {
		target.Output = declared.Calibration.Output
	}
	return target, observe
}

func knownPositive[T int | int64 | float64](value T) *T {
	if value > 0 {
		return &value
	}
	return nil
}
