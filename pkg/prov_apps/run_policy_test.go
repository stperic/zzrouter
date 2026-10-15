package prov_apps

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		protocol   config.AppProtocol
		want       RunLifecycle
	}{
		{"llamacpp", "on-demand", config.ProtocolOpenAI, LifecycleProcess},
		{"vllm", "on-demand", config.ProtocolOpenAI, LifecycleProcess},
		{"mlx", "on-demand", config.ProtocolOpenAI, LifecycleProcess},
		{"on-demand ollama", "on-demand", config.ProtocolOllama, LifecycleProcess},
		{"external ollama", "external", config.ProtocolOllama, LifecycleServed},
		{"service ollama", "service", config.ProtocolOllama, LifecycleServed},
		{"external openai", "external", config.ProtocolOpenAI, LifecycleNone},
		{"cloud", "cloud", config.ProtocolOpenAI, LifecycleNone},
		{"ollama-cloud", "cloud", config.ProtocolOllama, LifecycleNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Lifecycle(config.ServiceConfig{Mode: tc.mode, Protocol: tc.protocol}))
		})
	}
}

func TestDecide(t *testing.T) {
	held := func(status instance.Status, provider string) *RunView {
		return &RunView{ID: "held", Provider: provider, Status: status}
	}
	blocker := &RunView{ID: "b"}
	// joined is the run a start in flight produced, as a joiner of that start sees it.
	joined := func(status instance.Status, provider string) Node {
		return Node{Joined: true, Existing: &RunView{ID: "flown", Provider: provider, Status: status, Failure: errTestStart}}
	}
	for _, tc := range []struct {
		name    string
		want    Want
		node    Node
		act     Act
		run     string
		replace string
		err     error
		msg     string
	}{
		// A process held by another provider cannot change a non-process decision.
		{name: "none ready", want: Want{Force: true}, node: Node{Lifecycle: LifecycleNone, Reachable: true, Existing: held(instance.StatusStopping, "other")}, act: ActReady},
		{name: "none absent", node: Node{Lifecycle: LifecycleNone, Reachable: true, Absent: true}, err: ErrModelNotLocal},
		{name: "none unreachable", node: Node{Lifecycle: LifecycleNone, Absent: true}, err: ErrProviderUnavailable},
		// A served model is a run: only reachability is its own; the run rules are everyone's.
		{name: "served starts", node: Node{Lifecycle: LifecycleServed, Reachable: true}, act: ActStart},
		{name: "served reuses its run", node: Node{Lifecycle: LifecycleServed, Reachable: true, Existing: held(instance.StatusRunning, "fake")}, act: ActReuse, run: "held"},
		{name: "served refuses another provider's run", node: Node{Lifecycle: LifecycleServed, Reachable: true, Existing: held(instance.StatusRunning, "other")}, err: ErrRunHeldByOtherProvider},
		{name: "served force replaces its run", want: Want{Force: true}, node: Node{Lifecycle: LifecycleServed, Reachable: true, Existing: held(instance.StatusRunning, "fake")}, act: ActStart, replace: "held"},
		{name: "unload of a model with no run starts nothing", want: Want{Unload: true}, node: Node{Lifecycle: LifecycleServed, Reachable: true}, act: ActReady},
		{name: "unload of a stopping model goes to the provider", want: Want{Unload: true}, node: Node{Lifecycle: LifecycleServed, Reachable: true, Existing: held(instance.StatusStopping, "fake")}, act: ActReady},
		{name: "unload of another provider's model goes to the provider", want: Want{Unload: true}, node: Node{Lifecycle: LifecycleServed, Reachable: true, Existing: held(instance.StatusRunning, "other")}, act: ActReady},
		{name: "unload of a running model reuses it", want: Want{Unload: true}, node: Node{Lifecycle: LifecycleServed, Reachable: true, Existing: held(instance.StatusRunning, "fake")}, act: ActReuse, run: "held"},
		{name: "served stopping retries", node: Node{Lifecycle: LifecycleServed, Reachable: true, Existing: held(instance.StatusStopping, "other")}, err: instance.ErrStopping},
		{name: "served unreachable", node: Node{Lifecycle: LifecycleServed}, err: ErrProviderUnavailable},
		{name: "served unreachable before catalog", node: Node{Lifecycle: LifecycleServed, Absent: true}, err: ErrProviderUnavailable},

		// Shutdown answers first: a joiner whose flown run Stop is ending must not be told to retry.
		{name: "shutdown refuses a joiner of a stopping run", node: Node{ShuttingDown: true, Joined: true, Existing: &RunView{ID: "flown", Provider: "fake", Status: instance.StatusStopping}}, err: ErrShutdown},
		{name: "shutdown refuses a start", node: Node{ShuttingDown: true}, err: ErrShutdown},
		{name: "shutdown refuses a reuse", node: Node{ShuttingDown: true, Existing: held(instance.StatusRunning, "fake")}, err: ErrShutdown},
		{name: "shutdown refuses what the provider answers", node: Node{ShuttingDown: true, Lifecycle: LifecycleNone, Reachable: true}, err: ErrShutdown},
		{name: "nothing running starts", act: ActStart},
		{name: "missing catalog", node: Node{Absent: true}, err: ErrModelNotLocal},
		{name: "running reuse", node: Node{Existing: held(instance.StatusRunning, "fake")}, act: ActReuse, run: "held"},
		{name: "running reuse ignores catalog", node: Node{Absent: true, Existing: held(instance.StatusRunning, "fake")}, act: ActReuse, run: "held"},
		{name: "unhealthy reuse", node: Node{Existing: held(instance.StatusUnhealthy, "fake")}, act: ActReuse, run: "held"},
		{name: "restarting reuse", node: Node{Existing: held(instance.StatusRestarting, "fake")}, act: ActReuse, run: "held"},
		{name: "starting reuse", node: Node{Existing: held(instance.StatusStarting, "fake")}, act: ActReuse, run: "held"},
		{name: "stopping refuses until drain finishes", node: Node{Existing: held(instance.StatusStopping, "fake")}, err: instance.ErrStopping},
		{name: "force also refuses stopping", want: Want{Force: true}, node: Node{Existing: held(instance.StatusStopping, "fake")}, err: instance.ErrStopping},
		{name: "provider mismatch", node: Node{Existing: held(instance.StatusRunning, "other")}, err: ErrRunHeldByOtherProvider},
		{name: "force other provider replaces it", want: Want{Force: true}, node: Node{Existing: held(instance.StatusRunning, "other")}, act: ActStart, replace: "held"},
		{name: "force same provider replaces it", want: Want{Force: true}, node: Node{Existing: held(instance.StatusRunning, "fake")}, act: ActStart, replace: "held"},
		{name: "runtime mismatch", want: Want{Runtime: "other"}, node: Node{Existing: held(instance.StatusRunning, "fake")}, err: ErrInstancesRunning},
		{name: "plan mismatch", want: Want{Plan: "other"}, node: Node{Existing: held(instance.StatusRunning, "fake")}, err: ErrInstancesRunning},
		{name: "terminal cleanup", node: Node{Existing: held(instance.StatusFailed, "fake")}, act: ActStart, replace: "held"},
		{name: "force missing catalog preserves run", want: Want{Force: true}, node: Node{Absent: true, Existing: held(instance.StatusRunning, "fake")}, err: ErrModelNotLocal},

		{name: "smoke beside live run", want: Want{Origin: OriginInstall}, node: Node{Existing: held(instance.StatusRunning, "fake")}, err: ErrInstancesRunning},
		{name: "smoke beside unconfirmed drain", want: Want{Origin: OriginInstall}, node: Node{Existing: &RunView{ID: "held", Provider: "fake", Status: instance.StatusFailed, Unconfirmed: true}}, err: ErrInstancesRunning},
		{name: "smoke replaces confirmed terminal", want: Want{Origin: OriginInstall}, node: Node{Existing: held(instance.StatusFailed, "fake")}, act: ActStart, replace: "held"},
		{name: "smoke on another runtime", want: Want{Origin: OriginInstall, Runtime: "fake"}, node: Node{Runtime: "other"}, msg: "runtime being installed"},
		{name: "smoke before runtime is known", want: Want{Origin: OriginInstall, Runtime: "fake"}, act: ActStart},
		{name: "smoke ignores the catalog", want: Want{Origin: OriginInstall, Runtime: "fake"}, node: Node{Absent: true, Runtime: "fake"}, act: ActStart},
		{name: "smoke never queues behind blocker", want: Want{Origin: OriginInstall}, node: Node{Blocker: blocker}, err: errAdmissionBusy},
		{name: "smoke never queues on held slot", want: Want{Origin: OriginInstall}, node: Node{AdmissionHeld: true}, err: errAdmissionBusy},

		// A joiner of a start in flight gets that start's run, judged by its own want.
		{name: "joiner same want", node: joined(instance.StatusStarting, "fake"), act: ActReuse, run: "flown"},
		{name: "joiner another provider", node: joined(instance.StatusStarting, "other"), err: ErrRunHeldByOtherProvider},
		{name: "joiner runtime differs", want: Want{Runtime: "other"}, node: joined(instance.StatusStarting, "fake"), err: ErrInstancesRunning},
		{name: "joiner plan differs", want: Want{Plan: "other"}, node: joined(instance.StatusStarting, "fake"), err: ErrInstancesRunning},
		{name: "force joiner shares the flight's run", want: Want{Force: true}, node: joined(instance.StatusStarting, "fake"), act: ActReuse, run: "flown"},
		{name: "force joiner of another provider's flight is refused", want: Want{Force: true}, node: joined(instance.StatusRunning, "other"), err: ErrRunHeldByOtherProvider},
		{name: "failed flight wins over another provider", node: joined(instance.StatusFailed, "other"), err: errTestStart},
		{name: "joiner of a run already failed gets its cause", want: Want{Force: true}, node: joined(instance.StatusFailed, "fake"), err: errTestStart},
		{name: "joiner of a run already stopped gets why", node: joined(instance.StatusStopped, "other"), err: errTestStart},
		{name: "joiner of a stopping run retries", node: joined(instance.StatusStopping, "fake"), err: instance.ErrStopping},

		{name: "queue behind blocker", node: Node{Blocker: blocker}, act: ActQueue, run: "b"},
		{name: "queue on held slot", node: Node{AdmissionHeld: true}, act: ActQueue},
		{name: "blocker precedes held slot", node: Node{Blocker: blocker, AdmissionHeld: true}, act: ActQueue, run: "b"},
		{name: "absent precedes queue", node: Node{Absent: true, Blocker: blocker}, err: ErrModelNotLocal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.want.Key = instance.NewKey("", "model", "")
			if tc.want.Provider == "" {
				tc.want.Provider = "fake"
			}
			d := Decide(tc.want, tc.node)
			switch {
			case tc.err != nil:
				require.ErrorIs(t, d.Err, tc.err)
			case tc.msg != "":
				require.ErrorContains(t, d.Err, tc.msg)
			default:
				require.NoError(t, d.Err)
			}
			if errors.Is(d.Err, instance.ErrStopping) {
				var admission *instance.AdmissionError
				require.ErrorAs(t, d.Err, &admission)
				assert.Equal(t, instance.AdmissionRetryAfter, admission.RetryAfter)
			}
			want := tc.act
			if tc.err != nil || tc.msg != "" {
				want = ActRefuse
			}
			assert.Equal(t, want, d.Act)
			assert.Equal(t, tc.run, d.Run)
			assert.Equal(t, tc.replace, d.Replace)
		})
	}
}

func TestChooseEvictions(t *testing.T) {
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	candidates := []evictionCandidate{{"new", 4, now}, {"b", 3, now.Add(-time.Hour)}, {"a", 2, now.Add(-time.Hour)}}
	for _, tc := range []struct {
		name      string
		shortfall int64
		want      []string
	}{
		{"no shortage", 0, nil}, {"negative shortage", -1, nil},
		{"oldest and tiebreak", 4, []string{"a", "b"}},
		{"exact cover", 5, []string{"a", "b"}},
		{"one victim", 2, []string{"a"}},
		{"all victims", 9, []string{"a", "b", "new"}},
		{"not enough", 10, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, chooseEvictions(tc.shortfall, candidates))
			assert.Equal(t, "new", candidates[0].id)
		})
	}
	assert.Empty(t, chooseEvictions(1, nil))
}

func TestIdleExpired(t *testing.T) {
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name            string
		keepAlive, idle time.Duration
		zero, want      bool
	}{
		{name: "never", keepAlive: -1, idle: time.Hour},
		{name: "immediate", want: true},
		{name: "immediate without activity", zero: true, want: true},
		{name: "before", keepAlive: time.Minute, idle: time.Second},
		{name: "boundary", keepAlive: time.Minute, idle: time.Minute, want: true},
		{name: "after", keepAlive: time.Minute, idle: time.Minute + time.Nanosecond, want: true},
		{name: "no activity", keepAlive: time.Minute, zero: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			activity := now.Add(-tc.idle)
			if tc.zero {
				activity = time.Time{}
			}
			assert.Equal(t, tc.want, Expired(instance.IdleState{Status: instance.StatusRunning, Activity: activity, Released: true, KeepAlive: tc.keepAlive}, now))
		})
	}
}

func TestRegisteredRunKeepAlive(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{{"-1", -1}, {"0", 0}} {
		t.Run(tc.value, func(t *testing.T) {
			cfg := aliasTestConfig(t)
			require.NoError(t, cfg.UpdateApp("fake", func(svc *config.ServiceConfig) error { svc.Runtime.KeepAlive = tc.value; return nil }))
			mgr, err := NewProviderAppManager(cfg)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, mgr.Stop(context.Background())) })
			svc, _ := cfg.LookupApp("fake")
			run, err := mgr.registerRun(t.Context(), EnsureRequest{Provider: "fake", Model: "m"}, launchSelection{config: svc})
			require.NoError(t, err)
			assert.Equal(t, tc.want, run.GetKeepAlive())
			idle := run.IdleSnapshot()
			assert.Equal(t, false, Expired(idle, utils.Now()))
			// Registry visibility lasts until the reaper actually ends the run.
			assert.Contains(t, mgr.Instances().List(), run)
		})
	}
}

// An unset decision is never a silent success.
func TestUnsetDecisionIsAnError(t *testing.T) {
	_, err := preparedStart{}.answer()
	require.ErrorIs(t, err, errUndecided)
}

func TestExpiredState(t *testing.T) {
	now := utils.Now()
	base := instance.IdleState{Status: instance.StatusRunning, Activity: now.Add(-time.Hour), Released: true}
	for _, state := range []instance.IdleState{
		{Status: instance.StatusRunning, Activity: base.Activity, Released: true, Leased: true},
		{Status: instance.StatusRunning, Activity: base.Activity, Released: true, Claimed: true},
		{Status: instance.StatusStarting, Activity: base.Activity, Released: true},
		{Status: instance.StatusRunning, Activity: base.Activity, Released: true, Pinned: true},
		{Status: instance.StatusRunning, Activity: base.Activity, Released: true, Busy: true},
		{Status: instance.StatusStopped, Activity: base.Activity, Released: true},
	} {
		state.KeepAlive = time.Minute
		assert.False(t, Expired(state, now))
	}
	assert.True(t, Expired(base, now))
	base.Released = false
	assert.False(t, Expired(base, now), "zero must wait for release")
}
