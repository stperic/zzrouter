package servercli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/stperic/zzrouter/pkg/service"
	"github.com/stperic/zzrouter/pkg/update"
)

// newUpdateEnableCmd creates the command that brings an existing
// service install into the managed layout.
//
// Needed because the relocation otherwise only happens during first-run
// setup, and every node already in a fleet is past that. Without this
// the split would ship for nodes installed after it and leave the
// existing ones exactly as inert as before -- which is the entire
// problem it was written to fix.
func newUpdateEnableCmd() *cobra.Command {
	var restart bool

	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Move an existing service install into the managed layout",
		Long: `Bring a node that was installed before the managed layout up to date with it.

Moves the binaries into /opt/zzrouter/versions/<version>/bin, leaves symlinks
where they were, rewrites the unit file to run the stable path, and installs
the privileged updater (zzrouter-update.service, .path and .timer).

The install tree stays root-owned. The node runs unprivileged and asks the
updater rather than replacing its own binaries -- a service account that can
rewrite the binary an operator later runs under sudo is a path from a
compromised node to root on the machine.

Run as root on the node. New installs do this during first-run setup and do
not need it.`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return enableManagedUpdates(restart)
		},
	}

	cmd.Flags().BoolVar(&restart, "restart", true,
		"Restart the node so it runs from the managed path")

	return cmd
}

func enableManagedUpdates(restart bool) error {
	if os.Geteuid() != 0 {
		return errors.New("update enable rewrites the unit file and moves the node's binaries; run it as root")
	}

	svcMgr := service.NewServiceManager()
	if svcMgr.IsFirstRun() {
		return errors.New("no service install found here; run `zzrouter-node start` as root to install one")
	}

	// Same order as first-run setup, and for the same reason: the
	// directories have to exist before the binaries move into them, and
	// the binaries have to have moved before the unit file is written,
	// because that is what decides the ExecStart path.
	fmt.Print("  Setting up directories... ")
	if err := svcMgr.SetupDirectories(); err != nil {
		fmt.Println("failed")
		return fmt.Errorf("set up directories: %w", err)
	}
	fmt.Println("done")

	fmt.Print("  Placing binaries... ")
	nodePath, relocated, err := update.EnsureManagedLayout()
	switch {
	case errors.Is(err, update.ErrNotRelocatable):
		fmt.Println("failed")
		return fmt.Errorf("%w\n\nWithout the managed layout there is nowhere safe to install an update", err)
	case err != nil:
		fmt.Println("failed")
		return fmt.Errorf("place binaries: %w", err)
	case relocated:
		fmt.Printf("done (%s)\n", nodePath)
	default:
		fmt.Println("already in place")
	}

	fmt.Print("  Installing service and updater units... ")
	if err := svcMgr.InstallService(); err != nil {
		fmt.Println("failed")
		return fmt.Errorf("install service: %w", err)
	}
	fmt.Println("done")

	if !restart {
		// Worth saying rather than leaving to be discovered: until the
		// restart the node is still executing the binary from its old
		// path, so `update status` will still report it cannot update.
		fmt.Println("\nNot restarted: the running node is still on the old path.")
		fmt.Println("Run 'systemctl restart zzrouter-node' for this to take effect.")
		return nil
	}

	fmt.Print("  Restarting the node... ")
	if err := svcMgr.RestartService(); err != nil {
		fmt.Println("failed")
		return fmt.Errorf("restart the node: %w", err)
	}
	fmt.Println("done")

	fmt.Printf("\nUpdates are now performed by the privileged updater.\n")
	fmt.Printf("  Binaries:  %s\n", nodePath)
	fmt.Printf("  Requests:  %s\n", update.DefaultHandoff().RequestPath())
	fmt.Println("  Check it:  zzrouter-node update status")
	return nil
}
