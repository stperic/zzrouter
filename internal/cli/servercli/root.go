// Package servercli wires the zzrouter-node cobra tree and owns
// process-boot concerns (umask, panic net, early log-file routing) for
// the server binary. cmd/zzrouter-node/main.go is a 5-line delegation
// point.
package servercli

import (
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"

	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/pkg/observability/logger"
	"github.com/stperic/zzrouter/pkg/security"
	"github.com/stperic/zzrouter/pkg/service"
	"github.com/stperic/zzrouter/pkg/version"
)

// logFileEnvVar lets the detached-mode spawner tell the child where to
// write its rotating log. When unset, slog keeps its default os.Stderr
// handler — correct for attached/foreground runs and every subcommand
// that is not `start --detach`.
const logFileEnvVar = "ZZROUTER_LOG_FILE"

// Execute runs the zzrouter-node root command. Returns the exit code;
// callers (cmd/zzrouter-node/main.go) should os.Exit with it.
func Execute() int {
	_ = security.SetSecureUmask() // Startup installs the secure mask; the previous mask is not restored.

	// Early slog setup for detached children: route structured logs
	// through a size-rotating backend before any subsystem emits its
	// first slog call. Failures fall back silently to the default
	// stderr handler so a broken logs dir never blocks startup.
	if path := os.Getenv(logFileEnvVar); path != "" {
		if _, err := logger.UseFile(path, logger.FileBackendOptions{Compress: true}); err != nil {
			fmt.Fprintf(os.Stderr, "warn: could not open log file %s: %v\n", path, err)
		}
	}

	// Main-goroutine panic safety net. The full stack is captured by
	// logPanicSafely → slog (rotating file in detached mode, stderr
	// otherwise), so we always have a durable record. The stderr
	// label is best-effort human signal for attached runs — in
	// detached mode os.Stderr is /dev/null, so both the label and the
	// runtime's re-panic dump are discarded; the slog record is the
	// only surviving artifact. We keep the re-panic so attached runs
	// still get the runtime's native crash output and the process
	// exits with the correct non-zero status.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "zzrouter-node panic: %v\n", r)
			logPanicSafely(r)
			panic(r)
		}
	}()

	run := func() error { return newRootCmd().Execute() }

	if handled, err := service.RunUnderServiceManager(run); handled {
		if err != nil {
			return 1
		}
		return 0
	}

	if err := run(); err != nil {
		return 1
	}
	return 0
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "zzrouter-node",
		Short: "zzrouter-node - AI model router node",
		Long: `zzrouter-node runs a zzRouter cluster node (coordinator or worker).
It is a unified aggregation layer for large language model (LLM) inference providers,
routing OpenAI-compatible API requests to the appropriate provider by model name.`,
		Version:      version.Current.String(),
		SilenceUsage: true,
	}
	root.CompletionOptions.DisableDefaultCmd = true

	root.AddCommand(
		NewStartCmd(),
		NewStopCmd(),
		NewStatusCmd(),
		NewShowCmd(),
		NewConfigCmd(),
		NewClusterCmd(),
		NewProviderCmd(),
		NewVersionCmd(),
		NewUpdateCmd(),
		NewInstallPolicyCmd(),
	)

	return root
}

// logPanicSafely records a recovered panic via slog, swallowing any
// secondary panic from inside the slog path (e.g. a disk-full error
// during log rotation) so the outer deferred re-panic still reaches
// os.Exit with a non-zero code.
func logPanicSafely(r any) {
	defer func() { _ = recover() }()
	slog.Error("zzrouter-node panic", "error", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
}
