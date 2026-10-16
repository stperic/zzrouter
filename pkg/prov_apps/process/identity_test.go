package process

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdentityHelper(t *testing.T) {
	if !slices.Contains(os.Args, "zzrouter-identity-helper") {
		return
	}
	if slices.Contains(os.Args, "ignore-term") {
		signal.Ignore(syscall.SIGTERM)
	}
	_, _ = io.WriteString(os.Stdout, "ready\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func identityChild(t *testing.T, ignoreTerm ...bool) (int, int64, io.WriteCloser, <-chan struct{}) {
	t.Helper()
	binary, err := os.Executable()
	require.NoError(t, err)
	cmd := host.Command(binary, "-test.run=TestIdentityHelper", "--", "zzrouter-identity-helper")
	if len(ignoreTerm) > 0 && ignoreTerm[0] {
		cmd.Args = append(cmd.Args, "ignore-term")
	}
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	output, err := cmd.StdoutPipe()
	require.NoError(t, err)
	setProcAttributes(cmd)
	require.NoError(t, cmd.Start())
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("identity fixture was not reaped")
		}
	})
	ready, err := bufio.NewReader(output).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", ready)
	start, err := StartToken(cmd.Process.Pid)
	require.NoError(t, err)
	require.Positive(t, start)
	return cmd.Process.Pid, start, stdin, done
}

func TestOwnsIdentity(t *testing.T) {
	pid, start, _, _ := identityChild(t)
	for _, tc := range []struct {
		name string
		live int64
		err  error
		want bool
	}{
		{name: "same fork", live: start, want: true},
		{name: "recycled PID", live: start + 1},
		{name: "unreadable identity", err: os.ErrPermission},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inspect := Inspector{
				ReadStartToken: func(got int) (int64, error) { assert.Equal(t, pid, got); return tc.live, tc.err },
				Environ:        func(int32) ([]string, error) { t.Error("recorded identity must not read environment"); return nil, nil },
			}
			assert.Equal(t, tc.want, inspect.Owns(pid, start))
		})
	}
	assert.False(t, (Inspector{}).Owns(os.Getpid(), start))
	assert.False(t, (Inspector{}).Owns(1, start))
	assert.False(t, (Inspector{}).Owns(0, start))
}

func TestStartTokenStable(t *testing.T) {
	pid, first, _, _ := identityChild(t)
	second, err := StartToken(pid)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.True(t, (Inspector{}).Owns(pid, first))
}

func TestLegacyPIDUsesMarker(t *testing.T) {
	pid, _, _, _ := identityChild(t)
	identity := parsePIDFile([]byte(strconv.Itoa(pid)))
	require.Equal(t, pid, identity.PID)
	require.Zero(t, identity.StartToken)
	for _, tc := range []struct {
		name string
		env  []string
		err  error
		want bool
	}{
		{name: "marked", env: []string{ManagedProcessEnvKey + "=" + ManagedProcessEnvValue}, want: true},
		{name: "unmarked", env: []string{"PATH=/bin"}},
		{name: "empty procfs read", env: []string{""}},
		{name: "unsupported", err: errors.New("unsupported environment"), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			inspect := Inspector{Environ: func(got int32) ([]string, error) { reads++; assert.EqualValues(t, pid, got); return tc.env, tc.err }}
			assert.Equal(t, tc.want, inspect.Owns(identity.PID, identity.StartToken))
			assert.Equal(t, 1, reads)
		})
	}
}

func TestTerminateRefusesRecycledPID(t *testing.T) {
	pid, start, _, _ := identityChild(t)
	for _, group := range []int{0, pid} {
		req := TerminateRequest{PID: pid, StartToken: start, ProcessGroupID: group, Graceful: time.Millisecond, Inspector: Inspector{
			ReadStartToken: func(int) (int64, error) { return start + 1, nil },
		}}
		require.NoError(t, Terminate(t.Context(), req))
		require.True(t, ProcessExists(int32(pid)), "refused identity must survive graceful termination")
		require.NoError(t, forceKill(req))
		require.True(t, ProcessExists(int32(pid)), "refused identity must survive escalation")
	}
}

func TestTerminateRechecksIdentityAtEscalation(t *testing.T) {
	pid, start, _, _ := identityChild(t, true)
	reads := 0
	req := TerminateRequest{PID: pid, StartToken: start, Inspector: Inspector{
		ReadStartToken: func(int) (int64, error) {
			reads++
			if reads == 1 {
				return start, nil
			}
			return start + 1, nil
		},
	}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.NoError(t, Terminate(ctx, req))
	assert.Equal(t, 2, reads, "escalation must inspect identity again")
}

func TestTerminateAfterReap(t *testing.T) {
	pid, start, stdin, done := identityChild(t)
	require.NoError(t, stdin.Close())
	<-done
	require.False(t, ProcessExists(int32(pid)))
	require.NoError(t, Terminate(t.Context(), TerminateRequest{PID: pid, StartToken: start}))
}

func TestTerminateConcurrentReap(t *testing.T) {
	pid, start, stdin, done := identityChild(t)
	var reads atomic.Int32
	inspect := Inspector{Environ: func(int32) ([]string, error) { reads.Add(1); return []string{""}, nil }}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			assert.NoError(t, Terminate(t.Context(), TerminateRequest{PID: pid, StartToken: start, Inspector: inspect, Graceful: time.Second}))
		})
	}
	_ = stdin.Close()
	wg.Wait()
	<-done
	assert.Zero(t, reads.Load())
	assert.False(t, ProcessExists(int32(pid)))
}
