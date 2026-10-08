package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWritePIDFile_AtomicAndParseable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "abc.pid")

	in := pidInfo{
		PID:        1234,
		ChildPID:   5678,
		Provider:   "vllm",
		InstanceID: "abc",
		StartedAt:  "2026-04-13T00:00:00Z",
	}
	if err := writePIDFile(path, in); err != nil {
		t.Fatalf("writePIDFile: %v", err)
	}

	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file should be renamed away, stat err = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	var got pidInfo
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal pid file: %v", err)
	}
	if got != in {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, in)
	}
}

func TestExitCode_NilProcessState(t *testing.T) {
	cmd := &exec.Cmd{} // ProcessState nil
	if got := exitCode(cmd); got != 1 {
		t.Fatalf("exitCode(nil ProcessState) = %d, want 1", got)
	}
}
