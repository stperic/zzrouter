package instance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFailureSnapshot_PreservesCauseAndExit(t *testing.T) {
	inst := NewInstance("id", "provider", "arbitrary-model", 8000, 0, 0)
	inst.LogFilePath = filepath.Join(t.TempDir(), "run.log")
	require.NoError(t, os.WriteFile(inst.LogFilePath, []byte("Traceback (most recent call last):\nRuntimeError: incompatible runtime build\n"), 0600))
	inst.MarkFailed("readiness probe failure: RuntimeError:")
	code := 1
	inst.MarkFailedWithExit("process exited unexpectedly", &code, "")
	inst.MarkFailed("Process 42 is not running")
	inst.MarkUnhealthy("Health check failures")
	inst.MarkRunning()
	snapshot := inst.ToInfo("worker")
	assert.Equal(t, "RuntimeError: incompatible runtime build", snapshot.ErrorMessage)
	require.NotNil(t, snapshot.Failure)
	require.NotNil(t, snapshot.Failure.ExitCode)
	assert.Equal(t, 1, *snapshot.Failure.ExitCode)
	assert.Contains(t, snapshot.Failure.ErrorTail, "RuntimeError: incompatible runtime build")
	snapshot.Failure.ErrorTail[0] = "mutated"
	*snapshot.Failure.ExitCode = 77
	assert.NotContains(t, inst.ToInfo("worker").Failure.ErrorTail, "mutated")
	assert.Equal(t, 1, *inst.ToInfo("worker").Failure.ExitCode)
	inst.MarkStarting()
	assert.Nil(t, inst.ToInfo("worker").Failure)
}

func TestFailureSnapshot_ZeroAndUnknownExit(t *testing.T) {
	for _, tc := range []struct {
		name string
		code *int
	}{{"unknown", nil}, {"zero", new(int)}} {
		t.Run(tc.name, func(t *testing.T) {
			inst := NewInstance("id", "p", "m", 0, 0, 0)
			inst.MarkFailedWithExit("unexpected exit", tc.code, "")
			raw, err := json.Marshal(inst.ToInfo(""))
			require.NoError(t, err)
			if tc.code == nil {
				assert.Contains(t, string(raw), `"exit_code":null`)
			} else {
				assert.Contains(t, string(raw), `"exit_code":0`)
			}
		})
	}
}

func TestFailureSnapshot_BoundedRedactedErrorTail(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 0)
	inst.LogFilePath = filepath.Join(t.TempDir(), "run.log")
	secret := "sk-" + strings.Repeat("x", 24)
	log := strings.Repeat("padding without an error\n", 10000)
	log += "RuntimeError: prompt={private user payload}\n"
	log += strings.Repeat("ERROR: recoverable failure\n", 50)
	log += "\x1b[31mRuntimeError: token=" + secret + " URL https://user:private@host.invalid/path?arbitrary=private\x1b[0m\n"
	require.NoError(t, os.WriteFile(inst.LogFilePath, []byte(log), 0600))
	inst.MarkFailed("failed")
	info := inst.ToInfo("")
	require.NotNil(t, info.Failure)
	assert.True(t, info.Failure.Truncated)
	assert.LessOrEqual(t, len(info.Failure.ErrorTail), failureTailLines)
	text := strings.Join(info.Failure.ErrorTail, "\n")
	assert.LessOrEqual(t, len(text), failureTailBytes)
	for _, forbidden := range []string{secret, "private", "\x1b"} {
		assert.NotContains(t, text, forbidden)
	}
	assert.Contains(t, text, "[REDACTED]")
	assert.Contains(t, text, "[REDACTED URL]")
}

func TestFailureSnapshot_DropsPartialCredentialLine(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 0)
	inst.LogFilePath = filepath.Join(t.TempDir(), "run.log")
	require.NoError(t, os.WriteFile(inst.LogFilePath, []byte("RuntimeError: token="+strings.Repeat("secret", failureReadBytes)+"\nRuntimeError: final cause\n"), 0600))
	inst.MarkFailed("failed")
	text := strings.Join(inst.ToInfo("").Failure.ErrorTail, "\n")
	assert.Equal(t, "RuntimeError: final cause", text)
	assert.NotContains(t, text, "secret")
}

func TestFailureSnapshot_DiagnosticByteBudget(t *testing.T) {
	inst := NewInstance("id", "p", "m", 0, 0, 0)
	inst.LogFilePath = filepath.Join(t.TempDir(), "run.log")
	require.NoError(t, os.WriteFile(inst.LogFilePath, []byte(strings.Repeat("RuntimeError: "+strings.Repeat("x", 243)+"\n", 32)), 0600))
	inst.MarkFailed("failed")
	tail := strings.Join(inst.ToInfo("").Failure.ErrorTail, "\n")
	assert.LessOrEqual(t, len(tail), failureTailBytes)
}

func TestFailureSnapshot_StopWinsDiagnosis(t *testing.T) {
	for _, status := range []Status{StatusStopping, StatusStopped} {
		t.Run(string(status), func(t *testing.T) {
			inst := NewInstance("id", "p", "m", 0, 0, 0)
			inst.SetStatus(status)
			code := 1
			inst.MarkFailedWithExit("late failure", &code, "")
			assert.Equal(t, status, inst.GetStatus())
			assert.Nil(t, inst.ToInfo("").Failure)
		})
	}
}

func TestFailureSnapshot_QuotedCredentials(t *testing.T) {
	for _, message := range []string{`RuntimeError: response={"token":"invented-private-credential"}`, `ValueError: response={'password': 'invented-private-credential'}`, `RuntimeError: response={"access_token":"invented-private-credential","client_secret":"invented-private-credential"}`} {
		inst := NewInstance("id", "p", "m", 0, 0, 0)
		inst.LogFilePath = filepath.Join(t.TempDir(), "run.log")
		require.NoError(t, os.WriteFile(inst.LogFilePath, []byte(message+"\n"), 0600))
		inst.MarkFailed(message)
		raw, err := json.Marshal(inst.ToInfo(""))
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "invented-private-credential")
		assert.Contains(t, string(raw), "[REDACTED]")
	}
}
