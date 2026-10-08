package servercli

import (
	"fmt"
	"log/slog"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/update"
)

// updateGuard is one boot's relationship to an update that has been
// installed but not yet proven to run.
//
// The two halves of an update restart happen in different processes:
// the one that installs it exits, and only the one that starts
// afterwards can say whether the new binary works. This is that second
// half. Nil when no update is awaiting proof, which is the usual case.
type updateGuard struct {
	confirmer *update.Confirmer
	pending   *update.PendingConfirm
	probe     nodeProbe
}

// newUpdateGuard counts this boot against any pending update. The count
// is persisted before the server is built, because the failure being
// guarded against is the server never building at all.
func newUpdateGuard(cfg *pkgConfig.NodeConfig) *updateGuard {
	confirmer := update.NewConfirmer(cfg.Update.GetKeepPreviousVersions())

	pending, err := confirmer.ClaimBoot()
	if err != nil {
		slog.Warn("could not read the pending update record", "err", err)
		return nil
	}
	if pending == nil {
		return nil
	}
	return &updateGuard{confirmer: confirmer, pending: pending, probe: nodeProbe{cfg: cfg}}
}

// ExhaustedAttempts reports whether the new binary has now failed to
// come up often enough to give up on it, rolling back when it has.
// True means the caller should exit so the supervisor starts the
// restored binary.
func (g *updateGuard) ExhaustedAttempts() bool {
	if g == nil {
		return false
	}
	if g.pending.Attempts <= update.MaxConfirmAttempts {
		slog.Info("starting on a freshly installed version, pending confirmation",
			"from", g.pending.FromVersion,
			"to", g.pending.ToVersion,
			"attempt", g.pending.Attempts,
			"of", update.MaxConfirmAttempts,
		)
		return false
	}

	return g.rollback(fmt.Sprintf("it did not come up healthy in %d attempts", update.MaxConfirmAttempts))
}

// ConfirmWhenHealthy waits for the node to open its port and answer
// /health, then clears the pending record. If it never does, the update
// is rolled back and true is returned, meaning the process should exit
// so the supervisor starts the restored binary.
//
// The probe is the health endpoint rather than "the process stayed up",
// because the failure worth catching is a binary that runs but cannot
// serve.
func (g *updateGuard) ConfirmWhenHealthy(done <-chan struct{}) bool {
	if g == nil {
		return false
	}

	if err := g.probe.waitUntilServing(done); err != nil {
		// A shutdown for some unrelated reason is not a verdict on the
		// update. Leave the record alone and let the next boot count an
		// attempt and decide then.
		if g.stopped(done) {
			return false
		}
		return g.rollback(err.Error())
	}

	if err := g.confirmer.Commit(); err != nil {
		slog.Warn("node is healthy but the pending update record could not be cleared", "err", err)
		return false
	}
	slog.Info("update confirmed: the node is serving on the new version", "version", g.pending.ToVersion)
	return false
}

// stopped reports whether the node is shutting down for some reason
// other than this guard. Leave the record alone in that case: the next
// boot counts an attempt and decides then.
func (g *updateGuard) stopped(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// rollback restores the previous binary, and reports whether the caller
// should exit for it to take effect.
func (g *updateGuard) rollback(reason string) bool {
	// Same stake as the install path: only exit if something will start
	// the node again. Rolling back and exiting under a supervisor that
	// will not restart us trades a bad version for no version.
	if err := update.CanRestart(); err != nil {
		slog.Error("an update did not come up, but this node cannot restart itself; leaving it running and awaiting an operator",
			"to", g.pending.ToVersion, "reason", reason, "err", err)
		return false
	}

	if err := g.confirmer.Rollback(g.pending, reason); err != nil {
		// Nothing to restore, or the restore failed. Exiting would loop
		// on the same broken binary, so let the node try to run: a
		// degraded node beats a restart loop.
		slog.Error("could not roll back the failed update", "err", err)
		return false
	}
	return true
}
