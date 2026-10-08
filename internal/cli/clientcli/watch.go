package clientcli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	shared "github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
)

// RenderJobProgress subscribes to a server-side job and renders live
// progress on one rewriting status line, terminated by a summary line
// on done/failed. Ctrl-C fires CancelJob and waits for the terminal
// event before returning.
//
// label is the user-facing verb ("Deploying", "Launching", etc.) used
// in the status line.
//
// node is the owning-node hint forwarded to the stream endpoint; leave
// empty for coord-local jobs.
func RenderJobProgress(parent context.Context, client *pkgClient.Client, jobID, node, label string) error {
	if jobID == "" {
		return fmt.Errorf("watch: missing job_id (server did not return one)")
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// On first Ctrl-C: fire CancelJob, keep subscription open for the
	// terminal event. On second Ctrl-C: bail the subscription too.
	cancelled := false
	go func() {
		for range sigCh {
			if !cancelled {
				cancelled = true
				fmt.Print("\r\033[K")
				fmt.Printf("%s Cancel requested, waiting for server...\n", ui.GetInfoEmoji())
				// Detached so the cancel request isn't killed by the same Ctrl-C
				// unwinding parent; WithoutCancel keeps the parent's values.
				_ = client.CancelJob(context.WithoutCancel(parent), jobID)
				continue
			}
			cancel()
			return
		}
	}()

	lastLineLen := 0
	renderLine := func(s string) {
		if lastLineLen > 0 {
			fmt.Print("\r\033[K")
		}
		fmt.Print(s)
		lastLineLen = len(s)
	}

	fmt.Printf("%s %s (job %s)\n", ui.GetSearchEmoji(), label, jobID)

	terminal, err := client.SubscribeJob(ctx, jobID, node, func(ev pkgClient.JobEvent) {
		switch ev.Event {
		case pkgClient.JobEventError:
			// Connection-level error on the stream; surfaced on return.
			return
		case pkgClient.JobEventStreamClosed:
			return
		case pkgClient.JobEventEventsDropped:
			return
		}
		renderLine(formatJobProgressLine(label, ev))
	})

	if lastLineLen > 0 {
		fmt.Println()
		lastLineLen = 0
	}

	if err != nil {
		if ctx.Err() != nil {
			fmt.Printf("%s Aborted\n", ui.GetWarningEmoji())
			return nil
		}
		return fmt.Errorf("watch: %w", err)
	}

	if terminal == nil {
		fmt.Printf("%s Stream closed without a terminal event\n", ui.GetWarningEmoji())
		return nil
	}

	switch terminal.Phase {
	case "done":
		fmt.Printf("%s %s complete\n", ui.GetCheckEmoji(), label)
		return nil
	case "failed":
		msg := terminal.Err
		if msg == "" {
			msg = "job failed"
		}
		return fmt.Errorf("%s failed: %s", strings.ToLower(label), msg)
	default:
		fmt.Printf("%s %s ended in phase %q\n", ui.GetWarningEmoji(), label, terminal.Phase)
		return nil
	}
}

func formatJobProgressLine(label string, ev pkgClient.JobEvent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %s", label)
	if ev.Step != "" {
		fmt.Fprintf(&b, " · %s", ev.Step)
	}
	if ev.Percent > 0 {
		fmt.Fprintf(&b, " · %d%%", ev.Percent)
	}
	if ev.Bytes != nil && ev.Bytes.Total > 0 {
		fmt.Fprintf(&b, " · %s / %s",
			shared.FormatSize(ev.Bytes.Done),
			shared.FormatSize(ev.Bytes.Total))
	} else if ev.Bytes != nil && ev.Bytes.Done > 0 {
		fmt.Fprintf(&b, " · %s", shared.FormatSize(ev.Bytes.Done))
	}
	if ev.Phase != "" && ev.Phase != "running" {
		fmt.Fprintf(&b, " [%s]", ev.Phase)
	}
	return b.String()
}
