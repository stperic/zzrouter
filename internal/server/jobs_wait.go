package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/routing"
)

// jobAccess observes existing jobs; it owns no jobs or goroutine lifetime.
// Consumers provide their own context, including readiness and placement.
type jobAccess struct {
	registry *jobs.Registry
	local    func(string) bool
	proxy    *JobsProxy
	router   routing.Router
}

func (a *jobAccess) ownsNode(selector, reported string) bool {
	if selector == reported {
		return true
	}
	if a.local != nil && a.local(selector) {
		return a.local(reported)
	}
	if a.proxy != nil && a.proxy.ResolveEndpoint != nil {
		ep := a.proxy.ResolveEndpoint(selector)
		return ep != nil && ep.NodeName == reported
	}
	return false
}

func (a *jobAccess) Wait(ctx context.Context, id, node string) error {
	return a.Observe(ctx, id, node, func(ev jobs.Event) error {
		if ev.Phase == jobs.PhaseFailed {
			return fmt.Errorf("job %s failed: %s", id, ev.Err)
		}
		return nil
	})
}

func (a *jobAccess) Observe(ctx context.Context, id, node string, consume func(jobs.Event) error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	local := a.local == nil || a.local(node)
	owner := node
	var epoch string
	terminal := false
	var invalid error
	accept := func(ev jobs.Event) (bool, error) {
		if err := validateJobEvent(ev, id, owner, epoch); err != nil {
			invalid = err
			return false, err
		}
		epoch = ev.Epoch
		if ev.Dropped != nil {
			return false, nil
		}
		terminal = ev.Phase.IsTerminal()
		consumeErr := consume(ev)
		if consumeErr != nil {
			invalid = consumeErr
		}
		return terminal, consumeErr
	}
	var err error
	if local {
		if a.registry == nil {
			return errors.New("job registry unavailable")
		}
		// The registry establishes the canonical owner even for local aliases.
		ev, lookupErr := a.registry.Get(id)
		if lookupErr != nil {
			return lookupErr
		}
		owner = ev.Node
		err = a.observeLocal(ctx, id, accept)
	} else {
		if a.proxy == nil || a.proxy.ResolveEndpoint == nil {
			return errors.New("remote job access unavailable")
		}
		ep := a.proxy.ResolveEndpoint(node)
		if ep == nil {
			return fmt.Errorf("job owner %s unavailable", node)
		}
		owner = ep.NodeName
		base, baseErr := a.proxy.endpointURL(ep)
		if baseErr != nil {
			return baseErr
		}
		var resp *http.Response
		resp, err = a.proxy.openStream(ctx, internalJobsURL(base, "/"+url.PathEscape(id)+"/stream", nil))
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
				return fmt.Errorf("job stream %s returned invalid response: HTTP %d", id, resp.StatusCode)
			}
			err = jobs.ReadSSE(resp.Body, func(name string, raw []byte) (bool, error) {
				ev, closed, frameErr := jobFrame(name, raw)
				if frameErr != nil {
					invalid = frameErr
					return false, frameErr
				}
				if closed {
					return true, nil
				}
				return accept(ev)
			})
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if terminal || invalid != nil || errors.Is(err, bufio.ErrTooLong) {
		return err
	}
	// A closed/lost stream is not completion. Reconcile once, bounded by the
	// caller lifetime. Identity/shape failure never reaches this fallback.
	snapshotCtx, cancel := context.WithTimeout(ctx, constants.ClusterActionTimeout)
	defer cancel()
	ev, lookupErr := a.snapshot(snapshotCtx, id, owner, local)
	if lookupErr == nil && ev.Phase.IsTerminal() {
		_, acceptErr := accept(ev)
		if terminal || acceptErr != nil {
			return acceptErr
		}
	}
	return fmt.Errorf("job %s on %s: worker outcome unconfirmed and may still be running (stream: %v; snapshot: %v)", id, owner, err, lookupErr)
}

func (a *jobAccess) observeLocal(ctx context.Context, id string, accept func(jobs.Event) (bool, error)) error {
	sub, err := a.registry.Subscribe(id, jobs.SubscribeOptions{})
	if err != nil {
		return err
	}
	defer sub.Unsubscribe()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-sub.Events():
			if !ok {
				return nil
			}
			stop, err := accept(ev)
			if stop || err != nil {
				return err
			}
		}
	}
}

func (a *jobAccess) snapshot(ctx context.Context, id, owner string, local bool) (jobs.Event, error) {
	if local {
		return a.registry.Get(id)
	}
	ev, err := routeAndParse[jobs.Event](ctx, a.router, "GET", "/zzrouter/v1/internal/jobs/"+url.PathEscape(id), owner, nil)
	if err != nil {
		return jobs.Event{}, err
	}
	return *ev, nil
}

// jobFrame separates transport signals from authenticated job envelopes.
func jobFrame(name string, raw []byte) (jobs.Event, bool, error) {
	if name == "stream_closed" {
		return jobs.Event{}, true, nil
	}
	if name == "error" {
		var signal struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(raw, &signal) == nil && signal.Code == "node_disconnected" {
			return jobs.Event{}, true, nil
		}
	}
	var ev jobs.Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return ev, false, err
	}
	if (ev.Dropped != nil && name != "events_dropped") ||
		(ev.Dropped == nil && ((ev.Phase.IsTerminal() && name != "done") || (!ev.Phase.IsTerminal() && name != "progress"))) {
		return ev, false, errors.New("job stream event name does not match phase")
	}
	return ev, false, nil
}

func validateJobEvent(ev jobs.Event, id, owner, epoch string) error {
	if ev.JobID != id || ev.Node != owner || ev.Epoch == "" ||
		(epoch != "" && ev.Epoch != epoch) ||
		(ev.Phase != jobs.PhasePending && ev.Phase != jobs.PhaseRunning && !ev.Phase.IsTerminal()) {
		return fmt.Errorf("invalid job identity or phase for %s on %s", id, owner)
	}
	return nil
}
