package prov_apps

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/protocol"
)

// RunLifecycle describes how a provider makes a model ready.
type RunLifecycle int

const (
	LifecycleProcess RunLifecycle = iota // starts a process per model
	LifecycleServed                      // loads a model through its endpoint
	LifecycleNone                        // needs only catalog evidence
)

// Lifecycle derives run ownership from config, with process ownership first.
func Lifecycle(svc config.ServiceConfig) RunLifecycle {
	if svc.LaunchesProcess() {
		return LifecycleProcess
	}
	if svc.HasEndpoint() && protocol.LoadsModels(svc.Mode, svc.Protocol) {
		return LifecycleServed
	}
	return LifecycleNone
}

// Origin says what asked for a run.
type Origin int

const (
	OriginAPI     Origin = iota // a management call
	OriginRequest               // an inference request to a model with no run
	OriginInstall               // an install smoke, while the installer holds the runtime
)

// Want is one caller's demand on a run key (model and endpoint).
type Want struct {
	Model, Provider, Runtime, Plan string
	Force                          bool
	Origin                         Origin
}

// RunView is what policy sees of a registered run.
type RunView struct {
	ID, Provider, Runtime, Plan string
	Status                      instance.Status
	Unconfirmed                 bool // its stop never drained, so its runtime is still occupied
}

// Node is the evidence one decision is made on; a site sets what it has observed.
type Node struct {
	Lifecycle RunLifecycle
	Reachable bool
	Absent    bool     // the catalog says this node does not hold the model
	Runtime   string   // the runtime a start would use, once selected
	Existing  *RunView // the run holding the key
	// Blocker and AdmissionHeld are observed only for a launch measured against a memory budget.
	Blocker       *RunView // an admitted run still starting under a budget: its claim is not visible yet
	AdmissionHeld bool     // another launch is measuring
}

// Act is what the caller does with a decision.
type Act int

const (
	actUnset Act = iota // a decision nobody made; never acted on
	ActRefuse
	ActReady // the provider already serves the model
	ActLoad  // load the model through the provider's endpoint
	ActReuse // the existing run answers for the key
	ActStart // start a process, after stopping Replace
	ActQueue // wait on Run, or on the admission slot when Run is empty
)

// Decision is Decide's answer.
type Decision struct {
	Act     Act
	Run     string
	Replace string
	Err     error
}

func refuse(err error) Decision { return Decision{Act: ActRefuse, Err: err} }

// errUndecided answers a decision with no act and no reason instead of a silent success.
var errUndecided = errors.New("run policy returned no decision")

// Decide is the run policy: every rule on whether and how a want becomes a run.
func Decide(w Want, n Node) Decision {
	switch n.Lifecycle {
	case LifecycleNone:
		if !n.Reachable {
			return refuse(ErrProviderUnavailable)
		}
		if n.Absent {
			return refuse(fmt.Errorf("%w: %s is not in this provider's catalog; download or register it, using POST /zzrouter/v1/deployments for a cloud model", ErrModelNotLocal, w.Model))
		}
		return Decision{Act: ActReady}
	case LifecycleServed:
		if !n.Reachable {
			return refuse(ErrProviderUnavailable)
		}
		return Decision{Act: ActLoad}
	}
	smoke := w.Origin == OriginInstall
	replace := ""
	if run := n.Existing; run != nil {
		switch {
		case run.Status == instance.StatusStopping:
			// Drain may finish between retries; use the request-slot backoff.
			return refuse(&instance.AdmissionError{Reason: instance.ErrStopping, RetryAfter: instance.AdmissionRetryAfter})
		case smoke && (!run.Status.IsTerminal() || run.Unconfirmed):
			return refuse(ErrInstancesRunning)
		case w.Force || run.Status.IsTerminal():
			replace = run.ID
		case run.Provider != w.Provider:
			return refuse(fmt.Errorf("%w: %s is held by provider %s; requested %s", ErrRunHeldByOtherProvider, w.Model, run.Provider, w.Provider))
		case run.Runtime != w.Runtime || run.Plan != w.Plan:
			return refuse(ErrInstancesRunning)
		default:
			return Decision{Act: ActReuse, Run: run.ID}
		}
	}
	switch {
	case smoke && n.Runtime != "" && n.Runtime != w.Runtime:
		return refuse(errors.New("smoke model does not use the runtime being installed"))
	case !smoke && n.Absent: // a smoke tests the runtime being installed, not the catalog
		return refuse(fmt.Errorf("%w: %s; download it with auto_deploy before starting", ErrModelNotLocal, w.Model))
	case n.Blocker == nil && !n.AdmissionHeld:
		return Decision{Act: ActStart, Replace: replace}
	case smoke:
		return refuse(errAdmissionBusy) // its install holds the gate a queued run needs
	case n.Blocker != nil:
		return Decision{Act: ActQueue, Run: n.Blocker.ID, Replace: replace}
	}
	return Decision{Act: ActQueue, Replace: replace}
}

type evictionCandidate struct {
	id       string
	mib      int64
	activity time.Time
}

func chooseEvictions(shortfall int64, candidates []evictionCandidate) []string {
	var available int64
	for _, candidate := range candidates {
		available += candidate.mib
	}
	if shortfall <= 0 || available < shortfall {
		return nil
	}
	candidates = slices.Clone(candidates)
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].activity.Equal(candidates[j].activity) {
			return candidates[i].id < candidates[j].id
		}
		return candidates[i].activity.Before(candidates[j].activity)
	})
	var selected int64
	var ids []string
	for _, candidate := range candidates {
		ids = append(ids, candidate.id)
		selected += candidate.mib
		if selected >= shortfall {
			break
		}
	}
	return ids
}

// idleExpired is Ollama's keep-alive rule: below zero never, zero at once, else after keep-alive idle.
func idleExpired(keepAlive time.Duration, lastActivity, now time.Time) bool {
	return keepAlive == 0 || (keepAlive > 0 && !lastActivity.IsZero() && now.Sub(lastActivity) > keepAlive)
}
