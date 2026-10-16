// Package testproc bounds fixture processes by the test process that started
// them. Fake engines get their own process group, so killing a test's process
// group (a tool timeout, Ctrl-C) takes the test binary and any launcher with it
// and leaves an unbounded fake running forever.
//
// The owner is the test binary, never the fake's parent: a fake that ended with
// its launcher would hide a broken production kill path from the test.
package testproc

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

const ownerEnv = "TESTPROC_OWNER_PID" // not ZZROUTER_: provider environments refuse that prefix

// Script returns a /bin/sh fixture whose body may test `kill -0 $test_owner`,
// true while this test process runs. Every unbounded loop in a fake must test
// it. A variable, not a function: a trap that fires inside a function sees the
// function's positional parameters instead of the script's.
func Script(body string) string { return script(os.Getpid(), body) }

func script(owner int, body string) string {
	return fmt.Sprintf("#!/bin/sh\ntest_owner=%d\n%s", owner, body)
}

// OwnerEnv names this test process as the owner of a helper process it starts.
func OwnerEnv() (key, value string) { return ownerEnv, strconv.Itoa(os.Getpid()) }

// AwaitOwner blocks a helper process until the owner named by OwnerEnv exits.
func AwaitOwner() {
	owner, err := strconv.ParseInt(os.Getenv(ownerEnv), 10, 32)
	if err != nil {
		panic(fmt.Sprintf("testproc: %s must name the owning test process: %v", ownerEnv, err))
	}
	// Only a definite "gone" ends the helper; a failed lookup is not one.
	for {
		if alive, err := process.PidExists(int32(owner)); err == nil && !alive {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
