package servercli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Files the rotation command touches. Paths are resolved against the
// node's XDG config dir at runtime.
var rotateKeyTargets = []string{
	filepath.Join("ca", "ca.key"),
	filepath.Join("ca", "ca.pem"),
	filepath.Join("identity", "node.key"),
	filepath.Join("identity", "node.pem"),
}

// rotateKeyLockFile is the concurrent-rotation guard. Created with
// O_EXCL so a second rotation on the same config dir errors cleanly
// instead of racing the first one's backups.
const rotateKeyLockFile = ".rotate-key.lock"

// Sentinel errors — the runbook's exit-code table depends on these.
// runRotateKey maps each to its documented exit code before re-exiting.
var (
	errRotateConfirmMissing = errors.New("rotate-key refuses to run without --confirm; this operation is destructive (see docs/runbook_cluster_key_rotation.md)")
	errRotateNotCoordinator = errors.New("no ca/ca.key: this node is not a coordinator; re-pair the worker instead")
	errRotateConcurrent     = errors.New("rotation already in progress (or stale .rotate-key.lock in the config dir)")
)

// Exit codes. Stable across releases — the runbook references them.
const (
	exitCodeRotateOK             = 0
	exitCodeRotateFatal          = 1
	exitCodeRotateConfirmMissing = 2
	exitCodeRotateServerRunning  = 3
	exitCodeRotateNotCoordinator = 4
	exitCodeRotateConcurrent     = 5
)

func newClusterRotateKeyCmd() *cobra.Command {
	var confirm bool

	cmd := &cobra.Command{
		Use:   "rotate-key",
		Short: "Rotate the cluster mTLS CA key (destructive: forces re-pair of every worker)",
		Long: `Rotate the cluster mTLS certificate authority.

This backs up and removes the coordinator's CA keypair and its own
identity cert, then clears the worker endpoint list in node.yaml.
Every worker must re-pair after the next coordinator boot.

This command must run with the coordinator STOPPED. Rotation is
destructive and cannot be reversed without the on-disk .bak-<timestamp>
siblings it writes. Read docs/runbook_cluster_key_rotation.md before
using.`,
		Args:          cobra.NoArgs,
		SilenceErrors: true, // we map sentinel errors to stable exit codes below
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRotateKey(cmd, confirm)
		},
	}

	cmd.Flags().BoolVar(&confirm, "confirm", false,
		"required; acknowledges that rotation forces every worker to re-pair")

	return cmd
}

// runRotateKey is the command body. Maps sentinel errors to stable
// exit codes documented in docs/runbook_cluster_key_rotation.md.
func runRotateKey(cmd *cobra.Command, confirm bool) error {
	if !confirm {
		cmd.PrintErrln(errRotateConfirmMissing)
		os.Exit(exitCodeRotateConfirmMissing)
	}

	if err := checkNodeNotRunning(); err != nil {
		cmd.PrintErrln(err)
		os.Exit(exitCodeRotateServerRunning)
	}

	cm := pkgConfig.NewConfigManager("zzrouter")
	configDir := cm.GetNodeConfigDir()

	// Coordinator gate: the CA keypair only exists on coordinators.
	// Refusing to run on a worker avoids half-rotating identity-only
	// state that would leave the node in a non-recoverable hybrid.
	caKey := filepath.Join(configDir, "ca", "ca.key")
	if _, err := os.Stat(caKey); err != nil {
		cmd.PrintErrln(errRotateNotCoordinator)
		os.Exit(exitCodeRotateNotCoordinator)
	}

	if err := rotateKeyIn(cmd, configDir, utils.NowUTC()); err != nil {
		cmd.PrintErrln(err)
		if errors.Is(err, errRotateConcurrent) {
			os.Exit(exitCodeRotateConcurrent)
		}
		os.Exit(exitCodeRotateFatal)
	}
	return nil
}

// rotateKeyIn is the testable core — takes an explicit configDir and a
// fixed "now" so the test harness can assert deterministic backup paths.
func rotateKeyIn(cmd *cobra.Command, configDir string, now time.Time) error {
	// Filesystem-safe timestamp: RFC3339 contains ":" which breaks on
	// Windows volumes. Reduce to UTC basic-format.
	ts := now.Format("20060102T150405Z")

	// Concurrent-rotation guard. O_EXCL makes a second invocation fail
	// cleanly instead of racing the first one's moves.
	lockPath := filepath.Join(configDir, rotateKeyLockFile)
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("%w: %v", errRotateConcurrent, err)
	}
	_ = lock.Close()
	defer func() { _ = os.Remove(lockPath) }()

	// Back up + remove the CA + identity certs. Bak siblings land next
	// to the originals so the operator's recovery path is obvious.
	for _, rel := range rotateKeyTargets {
		src := filepath.Join(configDir, rel)
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("stat %s: %w", src, err)
		}
		dst := src + ".bak-" + ts
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("backup %s: %w", src, err)
		}
		cmd.Printf("backed up %s → %s\n", rel, filepath.Base(dst))
	}

	// Clear cluster.endpoints after backing up node.yaml. Stale
	// endpoints pointing at workers signed by the old CA are a footgun
	// after rotation — they re-pair to the new CA and the operator
	// thinks "cluster still looks healthy" until a bad actor re-uses
	// the old key.
	oldEndpoints, err := rotateKeyClearEndpoints(configDir, ts)
	if err != nil {
		return err
	}

	cmd.Println()
	cmd.Println("rotation complete.")
	cmd.Println("next steps:")
	cmd.Println("  1. start the coordinator: it regenerates ca.key+ca.pem on first boot.")
	cmd.Println("  2. re-pair every worker previously in the cluster:")
	if len(oldEndpoints) == 0 {
		cmd.Println("       (no prior endpoints recorded in node.yaml)")
	} else {
		for _, ep := range oldEndpoints {
			cmd.Printf("       - %s\n", ep)
		}
	}
	cmd.Println("  3. on each worker, remove cluster/ca.pem + cluster/coordinator_url before re-pairing.")
	cmd.Println("  4. keep the .bak-" + ts + " siblings until the new cluster is verified healthy.")
	return nil
}

// rotateKeyClearEndpoints rewrites node.yaml with an empty
// cluster.endpoints list, preserving every other field, and writes a
// .bak-<ts> sibling first. Returns the old endpoint list so the
// command body can print it as the operator's re-pair worklist.
func rotateKeyClearEndpoints(configDir, ts string) ([]string, error) {
	path := filepath.Join(configDir, "node.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	bak := path + ".bak-" + ts
	if err := os.WriteFile(bak, raw, 0o600); err != nil {
		return nil, fmt.Errorf("write backup %s: %w", bak, err)
	}

	cm := pkgConfig.NewConfigManager("zzrouter")
	cfg, err := cm.LoadNodeConfigFromDir(configDir)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	old := cfg.Cluster.Endpoints.Addresses()
	cfg.Cluster.Endpoints = nil

	if err := cm.SaveNodeConfigToDir(cfg, configDir); err != nil {
		return nil, fmt.Errorf("rewrite %s: %w", path, err)
	}
	return old, nil
}
