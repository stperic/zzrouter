package process

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"

	processinfo "github.com/shirou/gopsutil/v4/process"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

// ErrMemoryObservation means free device memory cannot be safely measured or selected.
var ErrMemoryObservation = errors.New("GPU memory observation unavailable")

// ErrMemoryBudgetInvalid identifies an invalid resolved budget parameter.
var ErrMemoryBudgetInvalid = errors.New("invalid GPU memory budget")

// MemoryHolder describes driver attribution without exposing process command lines.
type MemoryHolder struct {
	PID                  int    `json:"pid,omitempty"`
	MemoryMiB            *int64 `json:"memory_mib,omitempty"`
	Run                  string `json:"run,omitempty"`
	Model                string `json:"model,omitempty"`
	Ownership            string `json:"ownership"`
	Provider             string `json:"provider,omitempty"`
	UnusedCommitmentMiB  int64  `json:"unused_commitment_mib,omitempty"`
	ReclaimSkippedReason string `json:"reclaim_skipped_reason,omitempty"`
}

// RunRef identifies the zzRouter run behind a process, by id and the model it serves.
type RunRef struct {
	ID, Model, Provider string
	PID                 int
	Commitments         map[string]int64
}

// MemoryFitError refuses an engine's declared budget before the process starts.
type MemoryFitError struct {
	Sized             bool           `json:"-"`
	BudgetKind        string         `json:"budget_kind,omitempty"`
	Parameter         string         `json:"parameter"`
	Device            string         `json:"device"`
	RequiredMiB       int64          `json:"required_mib"`
	FreeMiB           int64          `json:"free_mib"`
	Holders           []MemoryHolder `json:"holders"`
	AttributionReason string         `json:"attribution_reason,omitempty"`
}

// Error names who holds the memory, so the reader can tell an idle zzRouter
// run it may stop from a workload only the operator can release.
func (e *MemoryFitError) Error() string {
	if e.Sized {
		return fmt.Sprintf("model context requirement %d MiB exceeds %s budget of %d MiB (%s)", e.RequiredMiB, e.BudgetKind, e.FreeMiB, e.Parameter)
	}
	if e.BudgetKind != "" {
		return fmt.Sprintf("model weights require %g GiB, exceeding %s reach of %g GiB (%s); choose a smaller quantization or another node", float64(e.RequiredMiB)/1024, e.BudgetKind, float64(e.FreeMiB)/1024, e.Parameter)
	}
	msg := fmt.Sprintf("GPU budget cannot fit on %s: required %d MiB, currently free %d MiB (%s)", e.Device, e.RequiredMiB, e.FreeMiB, e.Parameter)
	var held []string
	for _, h := range e.Holders {
		size := "size unknown"
		if h.MemoryMiB != nil {
			size = fmt.Sprintf("%d MiB", *h.MemoryMiB)
		}
		if h.Run != "" {
			identity := "serving " + h.Model
			if h.Provider != "" {
				identity = "provider " + h.Provider
			}
			detail := fmt.Sprintf("pid %d, %s", h.PID, size)
			if h.UnusedCommitmentMiB > 0 {
				detail = fmt.Sprintf("%d MiB unused commitment", h.UnusedCommitmentMiB)
			}
			if h.ReclaimSkippedReason != "" {
				detail += "; reclaim skipped: " + h.ReclaimSkippedReason
			}
			held = append(held, fmt.Sprintf("zzRouter run %s %s (%s)", h.Run, identity, detail))
		} else {
			held = append(held, fmt.Sprintf("a process zzRouter does not manage (pid %d, %s)", h.PID, size))
		}
	}
	switch {
	case len(held) > 0:
		msg += "; held by " + strings.Join(held, ", ")
	case e.AttributionReason != "":
		msg += "; holders unknown: " + e.AttributionReason
	}
	return msg
}

// Unwrap gives admission the same typed cause as an engine allocation failure.
func (e *MemoryFitError) Unwrap() error {
	return &httperr.EngineFailure{Class: httperr.FailureMemory, Message: e.Error()}
}

// maxMemoryDevices bounds device selection before any observation or allocation.
const maxMemoryDevices = 64

// MemoryAssessment separates admitted budgets from established model fit.
type MemoryAssessment struct {
	ContextMode             string `json:"context_mode"`
	ContextTokens           int64  `json:"context_tokens,omitempty"`
	AffordableContextTokens int64  `json:"affordable_context_tokens,omitempty"`
	Placement               string `json:"placement,omitempty"`
	Status                  string `json:"status"`
	Reason                  string `json:"reason"`
	BudgetKind              string `json:"budget_kind,omitempty"`
	RequiredMiB             int64  `json:"required_mib,omitempty"`
	CapacityMiB             int64  `json:"capacity_mib,omitempty"`
}

func (assessment *MemoryAssessment) setError(err error) {
	if err == nil {
		return
	}
	assessment.Status, assessment.Placement, assessment.Reason = "unknown", "", err.Error()
	var fit *MemoryFitError
	if errors.As(err, &fit) {
		assessment.Status, assessment.RequiredMiB, assessment.CapacityMiB = "no_fit", fit.RequiredMiB, fit.FreeMiB
	}
}

// NormalizeLegacy prevents old positive assessments from qualifying under the context contract.
func (assessment *MemoryAssessment) NormalizeLegacy() {
	if assessment.ContextMode == "" {
		*assessment = MemoryAssessment{Status: "unknown", ContextMode: "unknown", Reason: "node reported no context-aware capacity assessment"}
	}
}

// AdmitMemoryBudget shares capacity evidence and launch admission arithmetic.
func AdmitMemoryBudget(budget schema.MemoryBudget, need Need, parameters, environment map[string]string, devices []gpu.MemoryDevice, systemMiB int64, owners []RunRef, commitments map[string]int64, usableFraction ...string) (assessment MemoryAssessment, err error) {
	assessment = MemoryAssessment{ContextMode: need.ContextMode, ContextTokens: need.ContextTokens, Status: "unknown", Reason: "model fit is not established for this budget or sizing evidence", BudgetKind: budget.Kind}
	if assessment.ContextMode == "" {
		assessment.ContextMode = "unknown"
	}
	defer func() { assessment.setError(err) }()
	if budget.Kind == schema.MemoryExternal {
		return assessment, nil
	}
	if budget.Kind == schema.MemoryReserveMiB {
		return assessment, admitReserved(budget, parameters, environment, devices, owners, commitments)
	}
	fraction, auto, err := declaredFraction(budget, parameters, usableFraction)
	if err != nil {
		return assessment, err
	}
	selected, err := memoryDevicesForLaunch(devices, environment, parameters[budget.DeviceCountParameter], budget.Kind == schema.MemoryLayers && budget.DeviceCountParameter == "")
	if err != nil {
		return assessment, err
	}
	for _, device := range selected {
		if device.TotalMiB <= 0 || device.FreeMiB < 0 || device.FreeMiB > device.TotalMiB {
			return assessment, ErrMemoryObservation
		}
	}
	if err := weightMemoryFit(budget, need, selected, systemMiB, fraction, auto); err != nil {
		return assessment, err
	}
	sized := need.Total() > 0 && (budget.Kind == schema.MemoryFractionTotal || need.geometry != nil)
	if sized {
		assessment.RequiredMiB, assessment.CapacityMiB, assessment.Placement, err = sizedMemoryFit(budget, need, selected, systemMiB, fraction, auto, owners)
		assessment.AffordableContextTokens = affordableContext(budget, need, selected, systemMiB, fraction, auto, owners)
		if err != nil {
			return assessment, err
		}
		assessment.Status, assessment.Reason = "fit", "sized context requirement fits the assessed memory budget"
	}
	if budget.Kind != schema.MemoryFractionTotal {
		for _, device := range selected {
			fit := memoryFitError(budget.Parameter, device, device.FreeMiB, owners)
			if fit.FreeMiB < device.FreeMiB {
				return assessment, fit
			}
		}
		return assessment, nil
	}
	err = admitFraction(budget, need, parameters, selected, owners, fraction, auto, sized, &assessment)
	return assessment, err
}

func declaredFraction(budget schema.MemoryBudget, parameters map[string]string, usableFraction []string) (float64, bool, error) {
	value := parameters[budget.Parameter]
	auto := budget.Kind == schema.MemoryFractionTotal && config.IsAuto(value)
	if budget.Kind == schema.MemoryUnified && len(usableFraction) > 0 {
		value = usableFraction[0]
	}
	if auto || (budget.Kind != schema.MemoryFractionTotal && budget.Kind != schema.MemoryUnified) || (budget.Kind == schema.MemoryUnified && value == "") {
		return 0, auto, nil
	}
	fraction, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(fraction) || math.IsInf(fraction, 0) || fraction <= 0 || fraction > 1 {
		return 0, false, fmt.Errorf("%w: %s must be a finite fraction greater than zero and at most one", ErrMemoryBudgetInvalid, budget.Parameter)
	}
	return fraction, false, nil
}

func admitFraction(budget schema.MemoryBudget, need Need, parameters map[string]string, selected []gpu.MemoryDevice, owners []RunRef, fraction float64, auto, sized bool, assessment *MemoryAssessment) (err error) {
	if auto {
		if sized && need.ContextMode != "auto" {
			fraction = needFraction(need, selected)
		} else if fraction, err = autoFraction(budget, selected, owners); err != nil {
			return err
		}
	}
	if !sized {
		for _, device := range selected {
			required := int64(math.Ceil(float64(device.TotalMiB) * fraction))
			if assessment.CapacityMiB == 0 || device.FreeMiB-required < assessment.CapacityMiB-assessment.RequiredMiB {
				assessment.RequiredMiB, assessment.CapacityMiB = required, device.FreeMiB
			}
			fit := memoryFitError(budget.Parameter, device, required, owners)
			if required > fit.FreeMiB {
				return fit
			}
		}
	}
	if auto {
		parameters[budget.Parameter] = strconv.FormatFloat(fraction, 'f', 4, 64)
	}
	return nil
}

func affordableContext(budget schema.MemoryBudget, need Need, selected []gpu.MemoryDevice, systemMiB int64, fraction float64, auto bool, owners []RunRef) int64 {
	if need.ContextMode != "auto" || need.geometry == nil {
		return 0
	}
	low, high := int64(0), need.geometry.NativeContext
	for low < high {
		mid := low + (high-low)/2 + 1
		candidate, ok := need.atContext(mid)
		_, _, _, err := sizedMemoryFit(budget, candidate, selected, systemMiB, fraction, auto, owners)
		if ok && candidate.Total() > 0 && err == nil {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return low
}

// sizedMemoryFit is also the affordability predicate; placement and launch use the same rounded need.
func sizedMemoryFit(budget schema.MemoryBudget, need Need, selected []gpu.MemoryDevice, systemMiB int64, fraction float64, auto bool, owners []RunRef) (required, capacity int64, placement string, err error) {
	if budget.Kind != schema.MemoryFractionTotal {
		var total int64
		for _, device := range selected {
			total += device.TotalMiB
		}
		if budget.Kind == schema.MemoryUnified {
			if fraction == 0 {
				return 0, 0, "", ErrMemoryObservation
			}
			total = int64(float64(total) * fraction)
		}
		required = int64(math.Ceil(float64(need.Total()) * (1 + DefaultAutoHeadroomFraction)))
		capacity, placement = total, ""
		if budget.Kind == schema.MemoryLayers {
			placement = "gpu"
			if required > total && !selected[0].Unified {
				if systemMiB <= 0 {
					return required, 0, "", ErrMemoryObservation
				}
				capacity, placement = total+systemMiB, "offload"
			}
		}
		if required > capacity {
			err = &MemoryFitError{Sized: true, BudgetKind: budget.Kind, Parameter: budget.Parameter, RequiredMiB: required, FreeMiB: capacity}
		}
		return
	}
	if auto && budget.AutoSafetyMarginMiB < 0 {
		return 0, 0, "", ErrMemoryBudgetInvalid
	}
	sizedFraction := needFraction(need, selected)
	for _, device := range selected {
		amount := int64(math.Ceil(float64(device.TotalMiB) * sizedFraction))
		available := memoryFitError(budget.Parameter, device, amount, owners)
		if !auto && sizedFraction > fraction {
			return amount, int64(float64(device.TotalMiB) * fraction), "", &MemoryFitError{Sized: true, BudgetKind: budget.Kind, Parameter: budget.Parameter, Device: device.UUID, RequiredMiB: amount, FreeMiB: int64(float64(device.TotalMiB) * fraction)}
		}
		if auto {
			amount += safetyMargin(budget, device)
		} else if reserved := int64(math.Ceil(float64(device.TotalMiB) * fraction)); reserved > available.FreeMiB {
			available.RequiredMiB = reserved
			return reserved, available.FreeMiB, "", available
		}
		if capacity == 0 || available.FreeMiB-amount < capacity-required {
			required, capacity = amount, available.FreeMiB
		}
		if amount > available.FreeMiB {
			available.RequiredMiB = amount
			return amount, available.FreeMiB, "", available
		}
	}
	return
}

func weightMemoryFit(budget schema.MemoryBudget, need Need, selected []gpu.MemoryDevice, systemMiB int64, fraction float64, auto bool) error {
	reach := float64(0)
	for _, device := range selected {
		amount := float64(device.TotalMiB)
		if budget.Kind == schema.MemoryFractionTotal && !auto {
			amount *= fraction
		}
		reach += amount
	}
	if budget.Kind == schema.MemoryLayers && !selected[0].Unified {
		if systemMiB <= 0 {
			return nil // Missing RAM cannot prove the engine's reachable total.
		}
		reach += float64(systemMiB)
	}
	weightMiB := float64(need.WeightsMiB)
	if need.WeightsBytes > 0 {
		weightMiB = float64(need.WeightsBytes) / (1024 * 1024)
	}
	if weightMiB > reach {
		return &MemoryFitError{BudgetKind: budget.Kind, Parameter: budget.Parameter, Device: "reachable memory", RequiredMiB: int64(math.Ceil(weightMiB)), FreeMiB: int64(reach), Holders: []MemoryHolder{}}
	}
	return nil
}

// admitReserved admits a reserve_mib budget when every selected device has
// its reservation free, and records the reservation in commitments.
func admitReserved(budget schema.MemoryBudget, parameters, environment map[string]string, devices []gpu.MemoryDevice, owners []RunRef, commitments map[string]int64) error {
	reserved, err := PrepareMemoryBudget(budget, parameters, environment, devices)
	if err != nil {
		return err
	}
	for _, device := range devices {
		if required, selected := reserved[device.UUID]; selected {
			fit := memoryFitError(budget.Parameter, device, required, owners)
			if required > fit.FreeMiB {
				return fit
			}
		}
	}
	if commitments != nil {
		maps.Copy(commitments, reserved)
	}
	return nil
}

// memoryFitError names every process holding the device, marking zzRouter's own runs.
func memoryFitError(parameter string, device gpu.MemoryDevice, required int64, owners []RunRef) *MemoryFitError {
	failure := &MemoryFitError{Parameter: parameter, Device: device.UUID, RequiredMiB: required, FreeMiB: device.FreeMiB, Holders: []MemoryHolder{}, AttributionReason: device.ProcessReason}
	used := make(map[string]int64)
	for _, process := range device.Processes {
		holder := MemoryHolder{PID: process.PID, MemoryMiB: process.MemoryMiB, Ownership: "other_or_untracked"}
		if run, ok := runOwning(process.PID, owners); ok {
			holder.Run, holder.Model, holder.Ownership, holder.Provider = run.ID, run.Model, "zzrouter", run.Provider
			if process.MemoryMiB != nil {
				used[run.ID] += max(0, *process.MemoryMiB)
			}
		}
		failure.Holders = append(failure.Holders, holder)
	}
	for _, run := range owners {
		usage := used[run.ID]
		if device.ProcessReason != "" {
			usage = max(0, device.TotalMiB-device.FreeMiB)
		}
		unused := max(0, run.Commitments[device.UUID]-usage)
		if unused == 0 {
			continue
		}
		failure.FreeMiB -= unused
		failure.Holders = append(failure.Holders, MemoryHolder{MemoryMiB: &unused, Run: run.ID, Model: run.Model, Provider: run.Provider, Ownership: "zzrouter", UnusedCommitmentMiB: unused})
	}
	failure.FreeMiB = max(0, failure.FreeMiB)
	return failure
}

// runOwning returns the run whose process is pid or one of its ancestors: engines hold the GPU from
// child processes. An ancestry it cannot read is nobody's.
func runOwning(pid int, owners []RunRef) (RunRef, bool) {
	for seen := map[int]bool{}; pid > 0 && len(owners) > 0 && !seen[pid]; {
		for _, run := range owners {
			if run.PID == pid {
				return run, true
			}
		}
		seen[pid] = true
		parent, err := parentPID(int32(pid))
		if err != nil {
			return RunRef{}, false
		}
		pid = int(parent)
	}
	return RunRef{}, false
}

// parentPID reads a process's parent; tests replace it.
var parentPID = func(pid int32) (int32, error) {
	return (&processinfo.Process{Pid: pid}).Ppid()
}

func memoryDevicesForLaunch(devices []gpu.MemoryDevice, environment map[string]string, value string, allVisible bool) ([]gpu.MemoryDevice, error) {
	count := 1
	if value != "" {
		var err error
		count, err = strconv.Atoi(value)
		if err != nil || count < 1 || count > maxMemoryDevices {
			return nil, fmt.Errorf("%w: invalid device count", ErrMemoryBudgetInvalid)
		}
	}
	visible, configured := environment["CUDA_VISIBLE_DEVICES"]
	if !configured {
		if allVisible && len(devices) > 0 && len(devices) <= maxMemoryDevices {
			return devices, nil
		}
		// CUDA's default ordering need not match NVML on multi-GPU nodes.
		if len(devices) != 1 || count != 1 {
			return nil, fmt.Errorf("%w: multiple GPUs require explicit UUID visibility for budget admission", ErrMemoryObservation)
		}
		return devices, nil
	}
	selectors := strings.Split(visible, ",")
	if allVisible {
		count = len(selectors)
	}
	if len(selectors) < count || count > maxMemoryDevices {
		return nil, fmt.Errorf("%w: insufficient visible GPUs", ErrMemoryObservation)
	}
	selected := make([]gpu.MemoryDevice, 0, count)
	seen := make(map[string]bool)
	for _, selector := range selectors[:count] {
		selector = strings.TrimSpace(selector)
		var matches []gpu.MemoryDevice
		for _, device := range devices {
			// A numeric ordinal is unambiguous only on a single-GPU node.
			if selector == device.UUID || (len(devices) == 1 && selector == strconv.Itoa(device.Index)) {
				matches = append(matches, device)
			}
		}
		if len(matches) != 1 || seen[matches[0].UUID] {
			return nil, fmt.Errorf("%w: unknown or ambiguous GPU selector; use full GPU UUIDs", ErrMemoryObservation)
		}
		seen[matches[0].UUID] = true
		selected = append(selected, matches[0])
	}
	return selected, nil
}

// MiBPerGiB is the number of MiB in one GiB.
const MiBPerGiB int64 = 1024

// PrepareMemoryBudget validates and pins a reservation using the same snapshot for its complement.
func PrepareMemoryBudget(budget schema.MemoryBudget, params, env map[string]string, devices []gpu.MemoryDevice) (map[string]int64, error) {
	if budget.Kind != schema.MemoryReserveMiB {
		return nil, nil
	}
	selected, err := memoryDevicesForLaunch(devices, env, "1", false)
	if err != nil {
		return nil, err
	}
	amount, err := strconv.ParseInt(params[budget.Parameter], 10, 64)
	device := selected[0]
	if err != nil || amount <= 0 || amount > device.TotalMiB {
		return nil, fmt.Errorf("%w: %s must be positive MiB and at most device total (%d MiB)", ErrMemoryBudgetInvalid, budget.Parameter, device.TotalMiB)
	}
	if env != nil {
		env["CUDA_VISIBLE_DEVICES"] = device.UUID
	}
	if budget.ComplementParameter != "" {
		params[budget.ComplementParameter] = strconv.FormatFloat(float64(device.TotalMiB-amount)/float64(MiBPerGiB), 'f', -1, 64)
	}
	return map[string]int64{device.UUID: amount}, nil
}
