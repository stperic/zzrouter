//go:build linux

package process

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcEnvironExecHelper(t *testing.T) {
	if !slices.Contains(os.Args, "zzrouter-proc-exec") {
		return
	}
	if os.Args[len(os.Args)-1] == "old" {
		_, _ = io.WriteString(os.Stdout, "old\n")
		_, err := io.ReadFull(os.Stdin, make([]byte, 1))
		require.NoError(t, err)
		require.NoError(t, syscall.Exec("/proc/self/exe",
			[]string{"/proc/self/exe", "-test.run=TestProcEnvironExecHelper", "--", "zzrouter-proc-exec", "new"},
			[]string{ManagedProcessEnvKey + "=" + ManagedProcessEnvValue}))
		return
	}
	_, _ = io.WriteString(os.Stdout, "new\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestProcEnvironAcrossExec(t *testing.T) {
	binary, err := os.Executable()
	require.NoError(t, err)
	cmd := host.Command(binary, "-test.run=TestProcEnvironExecHelper", "--", "zzrouter-proc-exec", "old")
	cmd.Env = append(os.Environ(), ManagedProcessEnvKey+"="+ManagedProcessEnvValue)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("exec fixture was not reaped")
		}
	})
	reader := bufio.NewReader(stdout)
	phase, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "old\n", phase)
	pid := cmd.Process.Pid
	start, err := StartToken(pid)
	require.NoError(t, err)
	path := fmt.Sprintf("/proc/%d/environ", pid)
	fresh, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, slices.Contains(strings.Split(string(fresh), "\x00"), ManagedProcessEnvKey+"="+ManagedProcessEnvValue))
	stale, err := os.Open(path)
	require.NoError(t, err)
	defer stale.Close()
	_, err = stdin.Write([]byte("x"))
	require.NoError(t, err)
	phase, err = reader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "new\n", phase)
	require.True(t, ProcessExists(int32(pid)))
	currentStart, err := StartToken(pid)
	require.NoError(t, err)
	assert.Equal(t, start, currentStart, "exec preserves the fork identity")
	old, err := io.ReadAll(stale)
	require.NoError(t, err)
	assert.Zero(t, len(old), "an environment descriptor opened before exec returns EOF")
	fresh, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, slices.Contains(strings.Split(string(fresh), "\x00"), ManagedProcessEnvKey+"="+ManagedProcessEnvValue))
	assert.True(t, (Inspector{}).Owns(pid, start))
}
