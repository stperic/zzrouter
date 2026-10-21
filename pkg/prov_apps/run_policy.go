package prov_apps

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/protocol"
)

// RunLifecycle describes how a provider makes a model ready.
type RunLifecycle int

const (
	LifecycleProcess RunLifecycle = iota // starts a process per model
	LifecycleServed                      // loads a model into its daemon
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
	OriginRestore               // a recorded pin, using management-call policy
)

// Want is one caller's demand on a run key (model and endpoint).
type Want struct {
	Key                     instance.Key
	Provider, Runtime, Plan string
	Force                   bool
	Unload                  bool // the caller asks the model unloaded, by a zero keep-alive
	Origin                  Origin
}

// RunView is what policy sees of a registered run.
type RunView struct {
	ID, Provider, Runtime, Plan string
	Status                      instance.Status
	Unconfirmed                 bool  // its stop never drained, so its runtime is still occupied
	Failure                     error // why a terminal run ended
}

// Node is the evidence one decision is made on; a site sets what it has observed.
type Node struct {
	ShuttingDown bool // the manager is stopping: nothing starts, queues or joins
	Lifecycle    RunLifecycle
	Reachable    bool
	Absent       bool     // the catalog says this node does not hold the model
	Runtime      string   // the runtime a start would use, once selected
	Existing     *RunView // the run holding the key
	Joined       bool     // Existing is what a start in flight produced, and the caller joined that start
	// Blocker and AdmissionHeld are observed only for a launch measured against a memory budget.
	Blocker       *RunView // an admitted run still starting under a budget: its claim is not visible yet
	AdmissionHeld bool     // another launch is measuring
}

// Act is what the caller does with a decision.
type Act int

const (
	actUnset Act = iota // a decision nobody made; never acted on
	ActRefuse
	ActReady // the provider serves the model without a run
	ActReuse // the existing run answers for the key
	ActStart // start a run, after stopping Replace
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
	if n.ShuttingDown {
		return refuse(ErrShutdown)
	}
	switch n.Lifecycle {
	case LifecycleNone:
		if !n.Reachable {
			return refuse(ErrProviderUnavailable)
		}
		if n.Absent {
			return refuse(fmt.Errorf("%w: %s is not in this provider's catalog", ErrModelNotLocal, w.Key.Model))
		}
		return Decision{Act: ActReady}
	case LifecycleServed:
		if !n.Reachable {
			return refuse(ErrProviderUnavailable)
		}
	}
	smoke := w.Origin == OriginInstall
	replace := ""
	if n.Existing != nil {
		d := decideExisting(w, n, smoke)
		if d.Act != actUnset {
			return d
		}
		replace = d.Replace
	}
	switch {
	case w.Unload: // nothing to end: the provider answers for itself
		return Decision{Act: ActReady}
	case smoke && n.Runtime != "" && n.Runtime != w.Runtime:
		return refuse(errors.New("smoke model does not use the runtime being installed"))
	case !smoke && n.Absent: // a smoke tests the runtime being installed, not the catalog
		return refuse(fmt.Errorf("%w: %s", ErrModelNotLocal, w.Key.Model))
	case n.Blocker == nil && !n.AdmissionHeld:
		return Decision{Act: ActStart, Replace: replace}
	case smoke:
		return refuse(errAdmissionBusy) // its install holds the gate a queued run needs
	case n.Blocker != nil:
		return Decision{Act: ActQueue, Run: n.Blocker.ID, Replace: replace}
	}
	return Decision{Act: ActQueue, Replace: replace}
}

// decideExisting holds the rules for a run already on the key. An unset Act
// leaves the decision to Decide, starting after stopping Replace if set.
func decideExisting(w Want, n Node, smoke bool) Decision {
	run := n.Existing
	switch {
	case n.Joined && run.Status.IsTerminal():
		return refuse(run.Failure)
	case w.Unload && (run.Status == instance.StatusStopping || run.Provider != w.Provider):
		return Decision{Act: ActReady} // no run of this want to end: the provider answers for itself
	case run.Status == instance.StatusStopping:
		// Drain may finish between retries; use the request-slot backoff.
		return refuse(&instance.AdmissionError{Reason: instance.ErrStopping, RetryAfter: instance.AdmissionRetryAfter})
	case smoke && (!run.Status.IsTerminal() || run.Unconfirmed):
		return refuse(ErrInstancesRunning)
	case n.Joined && w.Force && run.Provider == w.Provider && run.Runtime == w.Runtime && run.Plan == w.Plan:
		return Decision{Act: ActReuse, Run: run.ID} // the flight did what Force asks; a second replacement would undo it
	case (w.Force && !n.Joined) || run.Status.IsTerminal():
		return Decision{Replace: run.ID}
	case run.Provider != w.Provider:
		return refuse(fmt.Errorf("%w: %s is held by provider %s; requested %s", ErrRunHeldByOtherProvider, w.Key.Model, run.Provider, w.Provider))
	case run.Runtime != w.Runtime || run.Plan != w.Plan:
		return refuse(ErrInstancesRunning)
	default:
		return Decision{Act: ActReuse, Run: run.ID}
	}
}

// memoryNeedsStop leaves unknown evidence to admission after stopping the replaced run.
func memoryNeedsStop(err error, bounds map[string]*int64) bool {
	var fit *process.MemoryFitError
	if errors.As(err, &fit) {
		return fit.BudgetKind == "" && !fit.CommitmentOnly && bounds[fit.Device] == nil
	}
	return errors.Is(err, process.ErrMemoryObservation) && !errors.Is(err, process.ErrMemorySelection)
}

type evictionCandidate struct {
	instance.IdleStopCandidate
	relief int64
}

// chooseEvictions selects the oldest candidates only when they cover the positive shortfall.
func chooseEvictions(shortfall int64, candidates []evictionCandidate) []instance.IdleStopCandidate {
	if shortfall <= 0 {
		return nil
	}
	candidates = slices.Clone(candidates)
	slices.SortFunc(candidates, func(a, b evictionCandidate) int {
		return cmp.Or(a.Activity.Compare(b.Activity), cmp.Compare(a.Instance.ID, b.Instance.ID))
	})
	var selected []instance.IdleStopCandidate
	for _, candidate := range candidates {
		if candidate.relief <= 0 {
			continue
		}
		selected = append(selected, candidate.IdleStopCandidate)
		shortfall -= candidate.relief
		if shortfall <= 0 {
			return selected
		}
	}
	return nil
}

// Expired is the idle rule: held leases, claims and pins never expire, zero expires after release.
func Expired(r instance.IdleState, now time.Time) bool {
	if r.EvictionBlockedReason() != "" {
		return false
	}
	return r.KeepAlive == 0 && r.Released || r.KeepAlive > 0 && !r.Activity.IsZero() && !now.Before(r.Activity.Add(r.KeepAlive))
}
