package utils

import (
	"log/slog"
	"runtime/debug"
)

// GoSafe spawns fn with panic recovery. Use instead of `go fn()` for
// long-running goroutines — a bare `go` with a panicking callee takes
// the whole process down. On panic, logs via slog.Error and exits;
// caller owns any restart loop.
//
// name is the slog "goroutine" attribute (e.g. "idle-reaper"). GoSafe
// does NOT track in any WaitGroup — pass a wg-aware closure:
//
//	wg.Add(1)
//	utils.GoSafe("worker", func() { defer wg.Done(); worker(ctx) })
func GoSafe(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("goroutine panicked",
					"goroutine", name,
					"panic", r,
					"stack", string(debug.Stack()),
				)
			}
		}()
		fn()
	}()
}

// RecoverAndLog is the deferred recover handler for callers composing
// recovery with their own defers. Order matters — recover MUST run
// before wg.Done so a panicking goroutine still decrements:
//
//	go func() {
//	    defer wg.Done()                   // runs SECOND
//	    defer utils.RecoverAndLog("name") // runs FIRST
//	    work()
//	}()
//
// recover() works only when called directly inside the deferred
// function, so the body is inlined rather than delegated.
func RecoverAndLog(name string) {
	if r := recover(); r != nil {
		slog.Error("goroutine panicked",
			"goroutine", name,
			"panic", r,
			"stack", string(debug.Stack()),
		)
	}
}
