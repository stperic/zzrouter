package main

import (
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stretchr/testify/require"
)

func TestLauncherStdinHelper(t *testing.T) {
	if os.Getenv("LAUNCHER_STDIN_HELPER") == "" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append(os.Args[:1], os.Args[i+1:]...)
			break
		}
	}
	flag.CommandLine = flag.NewFlagSet("launcher", flag.ExitOnError)
	os.Exit(run((*exec.Cmd).Start))
}

func TestLauncherStdinChildHelper(t *testing.T) {
	output := os.Getenv("LAUNCHER_STDIN_OUTPUT")
	if output == "" {
		return
	}
	data, err := io.ReadAll(os.Stdin)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(output, data, 0o600))
}

func TestLauncherChildStdin(t *testing.T) {
	binary, err := os.Executable()
	require.NoError(t, err)
	for _, tc := range []struct {
		name  string
		input string
	}{
		{name: "control pipe"},
		{name: "regular file", input: "engine input"},
		{name: "null device"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			output := filepath.Join(dir, "child-input")
			cmd := host.Command(binary, "-test.run=^TestLauncherStdinHelper$", "--",
				"--instance-id=stdin", "--provider=fixture", "--pid-dir", dir,
				"--", binary, "-test.run=^TestLauncherStdinChildHelper$")
			cmd.Env = append(os.Environ(), "LAUNCHER_STDIN_HELPER=1", "LAUNCHER_STDIN_OUTPUT="+output)
			var input io.Closer
			if tc.name == "control pipe" {
				input, err = cmd.StdinPipe()
				require.NoError(t, err)
			} else if tc.name == "regular file" {
				path := filepath.Join(dir, "input")
				require.NoError(t, os.WriteFile(path, []byte(tc.input), 0o600))
				file, openErr := os.Open(path)
				require.NoError(t, openErr)
				input, cmd.Stdin = file, file
			}
			t.Cleanup(func() {
				if input != nil {
					_ = input.Close()
				}
			})
			require.NoError(t, cmd.Start())
			done := make(chan struct{})
			var waitErr error
			go func() { waitErr = cmd.Wait(); close(done) }()
			t.Cleanup(func() {
				if input != nil {
					_ = input.Close()
				}
				select {
				case <-done:
				case <-time.After(8 * time.Second):
					_ = cmd.Process.Kill()
					<-done
					t.Error("launcher did not exit during cleanup")
				}
			})
			select {
			case <-done:
				require.NoError(t, waitErr)
			case <-time.After(5 * time.Second):
				t.Fatal("child must receive EOF while the launcher's control pipe stays open")
			}
			data, err := os.ReadFile(output)
			require.NoError(t, err)
			require.Equal(t, tc.input, string(data))
		})
	}
}
