package prov_apps

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeInstaller is a ProviderInstaller stub used to drive the async
// coordinator tests deterministically. Install/Upgrade/Uninstall block
// on `gate` until the test releases it, allowing precise control over
// in-flight vs completed state.
type fakeInstaller struct {
	name         string
	installed    atomic.Bool
	installErr   atomic.Value // error or nil
	uninstallErr atomic.Value
	upgradeErr   atomic.Value

	gate chan struct{} // released by the test to let Install/Upgrade/Uninstall return

	// call counters for assertion
	installCalls   atomic.Int32
	upgradeCalls   atomic.Int32
	uninstallCalls atomic.Int32
}

func newFakeInstaller(name string) *fakeInstaller {
	return &fakeInstaller{name: name, gate: make(chan struct{}, 8)}
}

// release lets one pending Install/Upgrade/Uninstall proceed. Buffered
// gate means back-to-back releases queue; callers can pre-arm for
// multi-step flows (Force=true runs Uninstall then Install).
func (f *fakeInstaller) release() {
	f.gate <- struct{}{}
}

func (f *fakeInstaller) ProviderName() string                  { return f.name }
func (f *fakeInstaller) IsInstalled() bool                     { return f.installed.Load() }
func (f *fakeInstaller) InstalledVersion() (string, error)     { return "1.0.0", nil }
func (f *fakeInstaller) SupportedPlatforms() []fsroot.Platform { return nil }
func (f *fakeInstaller) Rollback(ctx context.Context) error    { return nil }
func (f *fakeInstaller) InstallPlan(ctx context.Context, _ string) (*install.Plan, error) {
	return &install.Plan{}, nil
}
func (f *fakeInstaller) UpgradePlan(ctx context.Context, _ string) (*install.Plan, error) {
	return &install.Plan{}, nil
}
func (f *fakeInstaller) UninstallPlan(ctx context.Context) (*install.Plan, error) {
	return &install.Plan{}, nil
}
func (f *fakeInstaller) Preflight(ctx context.Context, _ *config.AppRequirements) *install.PreflightReport {
	return &install.PreflightReport{AllOK: true}
}

func (f *fakeInstaller) Install(ctx context.Context, _ string, _ func(string)) error {
	f.installCalls.Add(1)
	select {
	case <-f.gate:
	case <-ctx.Done():
		return ctx.Err()
	}
	if v := f.installErr.Load(); v != nil {
		if err, ok := v.(error); ok && err != nil {
			return err
		}
	}
	f.installed.Store(true)
	return nil
}

func (f *fakeInstaller) Upgrade(ctx context.Context, _ string, _ func(string)) error {
	f.upgradeCalls.Add(1)
	select {
	case <-f.gate:
	case <-ctx.Done():
		return ctx.Err()
	}
	if v := f.upgradeErr.Load(); v != nil {
		if err, ok := v.(error); ok && err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeInstaller) Uninstall(ctx context.Context) error {
	f.uninstallCalls.Add(1)
	select {
	case <-f.gate:
	case <-ctx.Done():
		return ctx.Err()
	}
	if v := f.uninstallErr.Load(); v != nil {
		if err, ok := v.(error); ok && err != nil {
			return err
		}
	}
	f.installed.Store(false)
	return nil
}

// newAsyncTestCoord wires a manager with jobs registry + fake installer
// injected into the coordinator's dispatcher. Returns the coord and fake.
func newAsyncTestCoord(t *testing.T, provider string) (*InstallCoordinator, *fakeInstaller, *jobs.Registry, func()) {
	t.Helper()
	reg, err := jobs.NewRegistry(jobs.Config{NodeName: "test", Clock: clock.System()})
	require.NoError(t, err)

	m, err := NewProviderAppManager(testAppsConfig(), WithJobsRegistry(reg))
	require.NoError(t, err)

	fake := newFakeInstaller(provider)
	m.Install().dispatcher.Register(provider, fake)

	cleanup := func() {
		_ = m.Stop(context.Background())
		reg.Stop()
	}
	return m.Install(), fake, reg, cleanup
}

// TestInstallAsync_HappyPath_Done — fresh install returns jobID sync,
// subscriber sees final done phase after fake installer releases gate.
func TestInstallAsync_HappyPath_Done(t *testing.T) {
	coord, fake, reg, cleanup := newAsyncTestCoord(t, "vllm")
	defer cleanup()

	finalizeCalled := atomic.Bool{}
	finalize := func() error {
		finalizeCalled.Store(true)
		return nil
	}

	jobID, err := coord.InstallAsync(context.Background(), InstallRequest{Provider: "vllm"}, finalize)
	require.NoError(t, err)
	require.NotEmpty(t, jobID)

	sub, err := reg.Subscribe(jobID, jobs.SubscribeOptions{})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	fake.release()

	// Drain events until terminal.
	terminal := waitTerminal(t, sub.Events(), 2*time.Second)
	assert.Equal(t, jobs.PhaseDone, terminal.Phase, "install must emit success phase")
	assert.True(t, finalizeCalled.Load(), "finalize callback must run before Done")
	assert.EqualValues(t, 1, fake.installCalls.Load())

	// After the goroutine exits, the live-slot must be released so a
	// subsequent install is NOT dedupe-returned with the same ID.
	require.NoError(t, coord.WaitForInstalls(ctxWithDeadline(t, 1*time.Second)))
}

// TestInstallAsync_FinalizeFail_Fails — when finalize returns error,
// handle emits Fail carrying "finalize: ..." so subscribers see the
// drift instead of a false Done.
func TestInstallAsync_FinalizeFail_Fails(t *testing.T) {
	coord, fake, reg, cleanup := newAsyncTestCoord(t, "vllm")
	defer cleanup()

	finalizeErr := errors.New("config write denied")
	finalize := func() error { return finalizeErr }

	jobID, err := coord.InstallAsync(context.Background(), InstallRequest{Provider: "vllm"}, finalize)
	require.NoError(t, err)

	sub, err := reg.Subscribe(jobID, jobs.SubscribeOptions{})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	fake.release()

	terminal := waitTerminal(t, sub.Events(), 2*time.Second)
	assert.Equal(t, jobs.PhaseFailed, terminal.Phase)
	assert.Contains(t, terminal.Err, "finalize")
}

// TestInstallAsync_DedupeReturnsSameID — second concurrent POST for the
// same provider returns the live jobID WITHOUT blocking on the per-name
// install mutex. H1 fix.
func TestInstallAsync_DedupeReturnsSameID(t *testing.T) {
	coord, fake, _, cleanup := newAsyncTestCoord(t, "vllm")
	defer cleanup()

	id1, err := coord.InstallAsync(context.Background(), InstallRequest{Provider: "vllm"}, nil)
	require.NoError(t, err)

	// Second call must return immediately (NOT block on lockProvider)
	// with the same ID.
	done := make(chan string, 1)
	go func() {
		id2, err := coord.InstallAsync(context.Background(), InstallRequest{Provider: "vllm"}, nil)
		require.NoError(t, err)
		done <- id2
	}()

	select {
	case id2 := <-done:
		assert.Equal(t, id1, id2, "second concurrent install must return the live jobID")
	case <-time.After(500 * time.Millisecond):
		t.Fatal("dedupe path blocked — second InstallAsync should return immediately with the live jobID")
	}

	fake.release()
	require.NoError(t, coord.WaitForInstalls(ctxWithDeadline(t, 2*time.Second)))
}

// TestInstallAsync_ForceReinstall_RunsUninstallThenInstall — M2: force
// path runs both phases inside the one goroutine under one handle.
func TestInstallAsync_ForceReinstall_RunsUninstallThenInstall(t *testing.T) {
	coord, fake, reg, cleanup := newAsyncTestCoord(t, "vllm")
	defer cleanup()

	fake.installed.Store(true) // simulate already-installed

	jobID, err := coord.InstallAsync(context.Background(), InstallRequest{Provider: "vllm", Force: true}, nil)
	require.NoError(t, err)

	sub, err := reg.Subscribe(jobID, jobs.SubscribeOptions{})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	// fake.Uninstall blocks first, then fake.Install blocks — need two
	// releases for the full Force=true sequence.
	fake.release()
	fake.release()

	terminal := waitTerminal(t, sub.Events(), 2*time.Second)
	assert.Equal(t, jobs.PhaseDone, terminal.Phase)
	assert.EqualValues(t, 1, fake.uninstallCalls.Load(), "force path must call Uninstall once")
	assert.EqualValues(t, 1, fake.installCalls.Load(), "force path must call Install once")
}

// TestInstallAsync_AlreadyInstalled_Sync409 — M3: when provider is
// installed and Force=false, return ErrProviderAlreadyInstalled
// synchronously. No job created, no goroutine spawned.
func TestInstallAsync_AlreadyInstalled_Sync409(t *testing.T) {
	coord, fake, _, cleanup := newAsyncTestCoord(t, "vllm")
	defer cleanup()

	fake.installed.Store(true)

	id, err := coord.InstallAsync(context.Background(), InstallRequest{Provider: "vllm"}, nil)
	assert.ErrorIs(t, err, ErrProviderAlreadyInstalled)
	assert.Empty(t, id, "no jobID when the request was synchronously refused")
	assert.EqualValues(t, 0, fake.installCalls.Load(), "installer must not run on AlreadyInstalled path")
}

// TestInstallAsync_Cancel_PropagatesViaHandleCtx — H2: DELETE /jobs/:id
// must cancel the running installer via handle.Context().
func TestInstallAsync_Cancel_PropagatesViaHandleCtx(t *testing.T) {
	coord, _, reg, cleanup := newAsyncTestCoord(t, "vllm")
	defer cleanup()

	jobID, err := coord.InstallAsync(context.Background(), InstallRequest{Provider: "vllm"}, nil)
	require.NoError(t, err)

	sub, err := reg.Subscribe(jobID, jobs.SubscribeOptions{})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	// Cancel instead of release — the fake installer's Install blocks on
	// `<-f.gate` with select on ctx.Done, so cancel must surface as
	// ctx.Err and terminate the job.
	require.NoError(t, reg.Cancel(jobID))

	terminal := waitTerminal(t, sub.Events(), 2*time.Second)
	assert.Equal(t, jobs.PhaseFailed, terminal.Phase, "cancelled install must emit Failed")
}

// TestWaitForInstalls_Drains — Stop path: pending install goroutine
// is awaited; drain completes after release.
func TestWaitForInstalls_Drains(t *testing.T) {
	coord, fake, _, cleanup := newAsyncTestCoord(t, "vllm")
	defer cleanup()

	_, err := coord.InstallAsync(context.Background(), InstallRequest{Provider: "vllm"}, nil)
	require.NoError(t, err)

	// Before release, WaitForInstalls with short deadline must timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err = coord.WaitForInstalls(ctx)
	cancel()
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	// Release and drain successfully.
	fake.release()
	err = coord.WaitForInstalls(ctxWithDeadline(t, 2*time.Second))
	assert.NoError(t, err)
}

// TestInstallAsync_PlatformMismatch_ErrUnsupportedPlatform pins the
// YAML-driven platform gate: ServiceConfig.Platforms is the single
// source of truth (no Go-side conditionals in builtins.go) and a
// mismatch returns ErrUnsupportedPlatform synchronously.
func TestInstallAsync_PlatformMismatch_ErrUnsupportedPlatform(t *testing.T) {
	coord, fake, _, cleanup := newAsyncTestCoord(t, "vllm")
	defer cleanup()

	require.NoError(t, coord.appsConfig().UpdateApp("vllm", func(sc *config.ServiceConfig) error {
		sc.Platforms = []config.InstallPlatform{{OS: "plan9", Arch: "alpha"}}
		return nil
	}))

	id, err := coord.InstallAsync(context.Background(), InstallRequest{Provider: "vllm"}, nil)
	assert.ErrorIs(t, err, install.ErrUnsupportedPlatform)
	assert.Empty(t, id, "no jobID when platform check fails")
	assert.EqualValues(t, 0, fake.installCalls.Load(), "installer must not run on platform mismatch")
}

// TestInstallAsync_PlatformEmpty_AllowsInstall confirms that absent or
// empty Platforms is unconstrained (default for ollama / llamacpp).
func TestInstallAsync_PlatformEmpty_AllowsInstall(t *testing.T) {
	coord, fake, _, cleanup := newAsyncTestCoord(t, "vllm")
	defer cleanup()

	id, err := coord.InstallAsync(context.Background(), InstallRequest{Provider: "vllm"}, nil)
	require.NoError(t, err)
	require.NotEmpty(t, id)
	fake.release()
}

// TestInstallAsync_NoJobsRegistry_ErrAsyncDisabled — guard: calling
// async entry without WithJobsRegistry is an internal invariant error.
func TestInstallAsync_NoJobsRegistry_ErrAsyncDisabled(t *testing.T) {
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	fake := newFakeInstaller("vllm")
	m.Install().dispatcher.Register("vllm", fake)

	_, err = m.Install().InstallAsync(context.Background(), InstallRequest{Provider: "vllm"}, nil)
	assert.ErrorIs(t, err, ErrAsyncDisabled)
}

// TestInstallAsync_NoJobsRegistry_DoesNotMaskAsAlreadyInstalled —
// infra-config error (jobs registry not wired) must surface as
// ErrAsyncDisabled even when the provider is already installed.
// Pins H2 from the post-code review: without the fix, the domain
// error ErrProviderAlreadyInstalled masked the real wiring bug and
// sent developers chasing 409 instead of 500-misconfig.
func TestInstallAsync_NoJobsRegistry_DoesNotMaskAsAlreadyInstalled(t *testing.T) {
	m, err := NewProviderAppManager(testAppsConfig())
	require.NoError(t, err)
	defer func() { _ = m.Stop(context.Background()) }()

	fake := newFakeInstaller("vllm")
	fake.installed.Store(true) // would cause AlreadyInstalled if checked first
	m.Install().dispatcher.Register("vllm", fake)

	_, err = m.Install().InstallAsync(context.Background(), InstallRequest{Provider: "vllm"}, nil)
	assert.ErrorIs(t, err, ErrAsyncDisabled, "infra-config error must win over domain state")
	assert.NotErrorIs(t, err, ErrProviderAlreadyInstalled)
	assert.NotErrorIs(t, err, ErrShutdown, "ErrAsyncDisabled must NOT be reported as ErrShutdown (H1 regression guard)")
}

// --- helpers ---

func ctxWithDeadline(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func waitTerminal(t *testing.T, ch <-chan jobs.Event, timeout time.Duration) jobs.Event {
	t.Helper()
	deadline := time.After(timeout)
	var last jobs.Event
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return last
			}
			last = ev
			if ev.Phase.IsTerminal() {
				return ev
			}
		case <-deadline:
			t.Fatalf("no terminal event within %v; last=%+v", timeout, last)
			return last
		}
	}
}

// Silence unused-import vet flags for sync/fmt in minor edits.
var _ = sync.WaitGroup{}
var _ = fmt.Sprintf
