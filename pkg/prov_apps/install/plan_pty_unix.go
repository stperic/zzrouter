//go:build !windows

package install

import (
	"bufio"
	"context"
	"syscall"
	"time"

	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/stperic/zzrouter/pkg/prov_apps/process"

	"github.com/creack/pty"
)

// runStepPTY starts cmd under a pseudo-terminal so TTY-detecting tools
// (pip, docker pull, curl --progress-bar) emit live byte-level updates.
// Progress-bar lines are newline-separated by replacing carriage
// returns with newlines before scanning so each "tick" of the bar
// becomes a discrete onLine invocation.
//
// Mirrors runStepStreaming for error reporting: retains a trailing
// tail of lines and embeds the last ones in the returned error on a
// non-zero exit.
func runStepPTY(ctx context.Context, cmd *exec.Cmd, onLine func(string)) error {
	if cmd.Stdin != nil {
		return runStepStreaming(ctx, cmd, onLine)
	}
	ptmx, tty, err := pty.Open()
	if err != nil {
		return fmt.Errorf("pty: %w", err)
	}
	defer func() { _ = ptmx.Close(); _ = tty.Close() }()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	cmd.WaitDelay = 2 * time.Second
	scanned := make(chan struct{})

	var tail tailBuffer
	// Route through a split-scanner that treats \r AND \n as
	// line terminators. pip's progress bar re-emits the same line
	// with \r; treating both as splits turns each tick into its own
	// onLine call the parser can consume.
	go func() {
		defer close(scanned)
		scanner := bufio.NewScanner(ptmx)
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		scanner.Split(splitCROrLF)
		for scanner.Scan() {
			line := strings.TrimRight(scanner.Text(), "\r\n")
			if line == "" {
				continue
			}
			tail.push(line)
			if onLine != nil {
				onLine(line)
			}
		}
		if err := scanner.Err(); err != nil && err != io.EOF {
			slog.Warn("pty scanner error", "err", err)
		}

	}()
	err = process.RunOwnedCommandStarted(ctx, cmd, func(int) { _ = tty.Close() })
	select {
	case <-scanned:
	case <-time.After(2 * time.Second):
		_ = ptmx.Close()
		<-scanned
	}
	if err != nil {
		return fmt.Errorf("%w: %s", err, tail.joined())
	}
	return nil
}

// splitCROrLF is a bufio.SplitFunc that breaks on either \r or \n.
// pip's TTY-mode progress bar uses \r-overwrites to refresh the same
// line; standard bufio.ScanLines only splits on \n and would buffer
// an entire bar's worth of history into a single token.
func splitCROrLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[0:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
