package process

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

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
	gpu.MemoryProcess
	Run       string `json:"run,omitempty"`
	Ownership string `json:"ownership"`
}

// MemoryFitError refuses an engine's declared budget before the process starts.
type MemoryFitError struct {
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
	msg := fmt.Sprintf("GPU budget cannot fit on %s: required %d MiB, currently free %d MiB (%s)", e.Device, e.RequiredMiB, e.FreeMiB, e.Parameter)
	var held []string
	for _, h := range e.Holders {
		size := "size unknown"
		if h.MemoryMiB != nil {
			size = fmt.Sprintf("%d MiB", *h.MemoryMiB)
		}
		if h.Run != "" {
			held = append(held, fmt.Sprintf("zzRouter run %s (pid %d, %s)", h.Run, h.PID, size))
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

// CheckMemoryBudget compares a resolved fraction-of-total budget on each selected GPU.
func CheckMemoryBudget(budget schema.MemoryBudget, parameters, environment map[string]string, devices []gpu.MemoryDevice, owners map[int]string) error {
	if budget.Kind != "fraction_total" {
		return nil
	}
	fraction, err := strconv.ParseFloat(parameters[budget.Parameter], 64)
	if err != nil || math.IsNaN(fraction) || math.IsInf(fraction, 0) || fraction <= 0 || fraction > 1 {
		return fmt.Errorf("%w: %s must be a finite fraction greater than zero and at most one", ErrMemoryBudgetInvalid, budget.Parameter)
	}
	count := 1
	if value := parameters[budget.DeviceCountParameter]; value != "" {
		count, err = strconv.Atoi(value)
		if err != nil || count < 1 || count > maxMemoryDevices {
			return fmt.Errorf("%w: invalid device count", ErrMemoryBudgetInvalid)
		}
	}
	selected, err := memoryDevicesForLaunch(devices, environment, count)
	if err != nil {
		return err
	}
	for _, device := range selected {
		required := int64(math.Ceil(float64(device.TotalMiB) * fraction))
		if required <= device.FreeMiB {
			continue
		}
		failure := &MemoryFitError{Parameter: budget.Parameter, Device: device.UUID, RequiredMiB: required, FreeMiB: device.FreeMiB, Holders: []MemoryHolder{}, AttributionReason: device.ProcessReason}
		for _, process := range device.Processes {
			holder := MemoryHolder{MemoryProcess: process, Ownership: "other_or_untracked"}
			if run := owners[process.PID]; run != "" {
				holder.Run, holder.Ownership = run, "zzrouter"
			}
			failure.Holders = append(failure.Holders, holder)
		}
		return failure
	}
	return nil
}

func memoryDevicesForLaunch(devices []gpu.MemoryDevice, environment map[string]string, count int) ([]gpu.MemoryDevice, error) {
	visible, configured := environment["CUDA_VISIBLE_DEVICES"]
	if !configured {
		// CUDA's default ordering need not match NVML on multi-GPU nodes.
		if len(devices) != 1 || count != 1 {
			return nil, fmt.Errorf("%w: multiple GPUs require explicit UUID visibility for budget admission", ErrMemoryObservation)
		}
		return devices, nil
	}
	selectors := strings.Split(visible, ",")
	if len(selectors) < count {
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
