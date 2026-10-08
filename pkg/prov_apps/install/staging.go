package install

import (
	"fmt"

	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
)

// StagedDir names the three directories a wholesale directory
// replacement moves between.
//
// Archive extraction overlays: tar and Expand-Archive write the files
// they carry and leave everything else alone. A provider upgraded in
// place therefore accumulates every artifact any earlier release ever
// shipped. That is not cosmetic where the payload is shared objects
// with versioned sonames — both llama.cpp and ollama bundle ggml, which
// discovers backends by scanning its directory, so a stale
// libggml.so.0.9.11 stays loadable for as long as the file exists.
// Overlaying also rewrites files a running process has mapped.
//
// So the payload is unpacked into Staging, proven there, and only then
// swapped into Live, with the displaced tree parked at Previous until
// the swap has succeeded.
//
// Live is replaced wholesale, so it must contain nothing the install
// does not produce. Where a provider keeps records beside its payload —
// ollama's version file, managed marker and serve.log sit in the same
// directory tree — stage the payload subdirectories and leave the
// records where they are.
type StagedDir struct {
	Live     string
	Staging  string
	Previous string
}

// PrepareUnixCommand returns a command leaving dir present and empty.
//
// Clearing Staging is safe in a way that clearing Live is not: nothing
// resolves against it, and no other step has run yet.
func PrepareUnixCommand(dir string) string {
	q := fsroot.ShellQuote(dir)
	return fmt.Sprintf("rm -rf %s && mkdir -p %s", q, q)
}

// PrepareWindowsCommand is PrepareUnixCommand's counterpart.
//
// Remove-then-create rather than New-Item -Force, because -Force leaves
// existing contents in place, which is the accumulation staging exists
// to stop.
func PrepareWindowsCommand(dir string) string {
	q := fsroot.PowerShellQuote(dir)
	return fmt.Sprintf("powershell -NoProfile -Command \"$ErrorActionPreference='Stop'; "+
		"if (Test-Path %s) { Remove-Item %s -Recurse -Force }; New-Item -ItemType Directory -Path %s -Force | Out-Null\"",
		q, q, q)
}

// ActivateUnixCommand swaps the staged tree into place.
//
// The ordering is the whole point, and it is why this is a swap rather
// than an `rm -rf` before extracting. Live is untouched until the staged
// tree has been proven, and Previous is deleted only once the move has
// succeeded, so every failure leaves a working install: either Live was
// never moved, or the rollback puts it back. Clearing Live up front
// instead would turn a failed download into an outage, and a
// multi-artifact install unpacks several archives into one directory, so
// that failure is reachable.
//
// Moving directories also leaves the old inodes intact, which matters
// when a process is running out of the tree being replaced.
func (s StagedDir) ActivateUnixCommand() string {
	live := fsroot.ShellQuote(s.Live)
	stage := fsroot.ShellQuote(s.Staging)
	prev := fsroot.ShellQuote(s.Previous)
	return fmt.Sprintf(
		"rm -rf %s; if [ -d %s ]; then mv %s %s; fi; "+
			"if mv %s %s; then rm -rf %s; else if [ -d %s ]; then mv %s %s; fi; exit 1; fi",
		prev, live, live, prev, stage, live, prev, prev, prev, live)
}

// ActivateWindowsCommand is ActivateUnixCommand's counterpart; see it for
// the ordering rationale.
//
// A Move-Item over a directory a running process holds open fails with a
// sharing violation, exactly as an in-place Expand-Archive -Force would,
// so this trades no new failure mode for the staging guarantee.
func (s StagedDir) ActivateWindowsCommand() string {
	live := fsroot.PowerShellQuote(s.Live)
	stage := fsroot.PowerShellQuote(s.Staging)
	prev := fsroot.PowerShellQuote(s.Previous)
	// $ErrorActionPreference='Stop' is what makes the rest of this
	// mean anything. PowerShell's file cmdlets report failure
	// non-terminatingly by default, so without it the catch never fires:
	// a Move-Item that failed to swap the tree would fall through to the
	// cleanup and exit 0, reporting a swap that did not happen as
	// success. With it, every step here fails loudly except the one
	// explicitly opted out below.
	//
	// That one is the trailing cleanup. By then the swap has happened
	// and the live tree is the new one, so a directory that could not be
	// deleted is garbage to collect, not a failed install. Windows
	// refuses to delete a running .exe (it allows renaming one), so a
	// daemon still holding the old binary would otherwise fail an
	// install that entirely succeeded.
	return fmt.Sprintf("powershell -NoProfile -Command \"$ErrorActionPreference='Stop'; "+
		"if (Test-Path %s) { Remove-Item %s -Recurse -Force }; "+
		"if (Test-Path %s) { Move-Item %s %s }; "+
		"try { Move-Item %s %s } catch { if (Test-Path %s) { Move-Item %s %s }; throw }; "+
		"if (Test-Path %s) { Remove-Item %s -Recurse -Force -ErrorAction SilentlyContinue }\"",
		prev, prev, live, live, prev, stage, live, prev, prev, live, prev, prev)
}

// ActivateUnixVerify composes the check an activation step carries.
// liveProbe is a shell command that exits 0 when the activated tree is
// good; the caller supplies it because only the caller knows what its
// payload should be able to do.
//
// Two properties, both load-bearing.
//
// It is the one durable check in a staged plan: every earlier step is
// scoped to a directory the swap consumes, so those are marked Transient
// and this is what VerifyAll leans on to say the install is intact.
//
// And it must be FALSE while Staging still exists. The single-step
// executor skips any step whose Verify already passes, so a check that
// only probed the live tree would be satisfied by the install being
// REPLACED — a guided upgrade would skip the swap and leave the staged
// tree unused.
func (s StagedDir) ActivateUnixVerify(liveProbe string) StepVerify {
	return ActivateUnixVerify(s.Staging, liveProbe)
}

// ActivateUnixVerify is the same check for a plan whose staging root is
// not one StagedDir's Staging: a payload spanning several directories
// activates them all from a single root, and it is that root's absence
// that says the activation finished.
func ActivateUnixVerify(stagingRoot, liveProbe string) StepVerify {
	return StepVerify{
		Type:     "command_output",
		Command:  fmt.Sprintf("test ! -d %s && %s && echo ok", fsroot.ShellQuote(stagingRoot), liveProbe),
		Expected: "ok",
	}
}

// ActivateWindowsVerify is ActivateUnixVerify's counterpart. liveProbe is
// a PowerShell fragment that throws or sets a non-zero exit code when the
// activated tree is bad.
func (s StagedDir) ActivateWindowsVerify(liveProbe string) StepVerify {
	return ActivateWindowsVerify(s.Staging, liveProbe)
}

// ActivateWindowsVerify is ActivateUnixVerify's counterpart for a payload
// activated from a single staging root.
func ActivateWindowsVerify(stagingRoot, liveProbe string) StepVerify {
	return StepVerify{
		Type: "command_output",
		Command: fmt.Sprintf("powershell -NoProfile -Command \"if (Test-Path %s) { exit 1 }; %s; Write-Output 'ok'\"",
			fsroot.PowerShellQuote(stagingRoot), liveProbe),
		Expected: "ok",
	}
}
