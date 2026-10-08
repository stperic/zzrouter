// Package mcp holds Model Context Protocol transport helpers.
//
// Scope is transport only: this package does not implement an MCP
// tool registry, ACLs, OpenAPI→MCP conversion, or any orchestration
// above the wire protocol. zzrouter's /mcp/{name}/* gateway uses
// these helpers to spawn and bridge stdio-transport MCP servers and
// to validate the per-server config loaded from node.yaml.
//
// Types in this file are safe for concurrent use.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"

	"github.com/stperic/zzrouter/pkg/host"
)

// StdioBridge owns one long-lived child process speaking MCP over
// stdio and serializes JSON-RPC line exchanges across its pipes.
//
// Serialization is per-bridge: two concurrent Exchange calls on the
// same bridge run one at a time so the line framing the MCP spec
// requires cannot interleave. Callers that want pipelining should
// maintain one bridge per logical request queue.
type StdioBridge struct {
	name   string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader

	writeMu sync.Mutex
	dead    atomic.Bool
}

// StartStdioBridge spawns the configured command, sets up the stdio
// pipes, and returns a ready-to-use bridge. The child runs until
// either it exits on its own or Close is called.
func StartStdioBridge(name, command string, args []string, env map[string]string) (*StdioBridge, error) {
	if command == "" {
		return nil, errors.New("mcp stdio: empty command")
	}
	cmd := host.Command(command, args...)
	cmd.Env = buildEnv(env)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp stdio: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("mcp stdio: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("mcp stdio: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp stdio: start %q: %w", command, err)
	}

	b := &StdioBridge{
		name:   name,
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
	}
	go drainStderr(name, stderr)
	go b.watch()
	return b, nil
}

// Exchange writes one JSON-RPC request line to stdin and reads one
// response line from stdout. Context cancellation closes stdin,
// which unblocks any in-flight read; the caller is expected to
// handle the resulting error and respawn via StartStdioBridge.
func (b *StdioBridge) Exchange(ctx context.Context, request []byte) (string, error) {
	if b == nil {
		return "", errors.New("mcp stdio: nil bridge")
	}
	if b.dead.Load() {
		return "", errors.New("mcp stdio: bridge is dead; respawn required")
	}

	req := make([]byte, 0, len(request)+1)
	req = append(req, bytes.TrimRight(request, "\n")...)
	req = append(req, '\n')

	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = b.stdin.Close()
		case <-done:
		}
	}()

	if _, err := b.stdin.Write(req); err != nil {
		b.dead.Store(true)
		return "", fmt.Errorf("mcp stdio: write: %w", err)
	}

	line, err := b.stdout.ReadString('\n')
	if err != nil {
		b.dead.Store(true)
		return "", fmt.Errorf("mcp stdio: read: %w", err)
	}
	return line, nil
}

// Alive reports whether the bridge is still usable.
func (b *StdioBridge) Alive() bool {
	return b != nil && !b.dead.Load()
}

// Close terminates the child process and releases its pipes. Safe to
// call multiple times; subsequent calls are no-ops. This is the
// cleanup path invoked by the gateway on server shutdown so
// long-lived MCP server processes do not outlive zzrouter.
func (b *StdioBridge) Close() error {
	if b == nil {
		return nil
	}
	if b.dead.Swap(true) {
		return nil
	}
	_ = b.stdin.Close()
	if b.cmd != nil && b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
	}
	return nil
}

func (b *StdioBridge) watch() {
	_ = b.cmd.Wait()
	b.dead.Store(true)
}

func buildEnv(extra map[string]string) []string {
	env := append([]string(nil), os.Environ()...)
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// drainStderr forwards the child's stderr to the zzrouter log at
// debug level. A 1 MiB token size lets servers emit large error
// payloads without silently stalling on the default 64 KiB Scanner
// limit — which would otherwise fill the stderr pipe and block the
// child on its next write.
func drainStderr(name string, stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		slog.Debug("mcp stdio stderr", "server", name, "line", scanner.Text())
	}
}
