package logger

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/natefinch/lumberjack.v2"
)

// FileBackendOptions tunes the rotating log backend. Zero-value fields for
// MaxSizeMB/MaxBackups/MaxAgeDays get default values that mirror
// pkg/prov_apps/logging.DefaultManagerConfig; Compress is a plain bool with
// no default coercion (Go zero-value false) — callers must pass true
// explicitly if they want gzip'd backups.
type FileBackendOptions struct {
	MaxSizeMB  int  // default 100
	MaxBackups int  // default 3
	MaxAgeDays int  // default 7
	Compress   bool // caller-specified; no default
}

// UseFile reconfigures the default slog handler to write through a
// size-rotating file backend. Call this once during startup, after config
// is loaded and before the first slog call in the serving path.
//
// Not safe for concurrent use — this mutates the package-level default
// logger. Calling from multiple goroutines is undefined and should not
// be necessary; the intended call site is early in main() before any
// subsystem spins up goroutines that log.
//
// Returns the file backend so callers can redirect raw writes (panic
// stacks, pre-slog fmt.Fprintln(os.Stderr, …)) into the same rotation
// scheme. The caller is responsible for os.Stderr redirection if desired.
func UseFile(path string, opts FileBackendOptions) (io.Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if opts.MaxSizeMB == 0 {
		opts.MaxSizeMB = 100
	}
	if opts.MaxBackups == 0 {
		opts.MaxBackups = 3
	}
	if opts.MaxAgeDays == 0 {
		opts.MaxAgeDays = 7
	}
	w := &lumberjack.Logger{
		Filename:   path,
		MaxSize:    opts.MaxSizeMB,
		MaxBackups: opts.MaxBackups,
		MaxAge:     opts.MaxAgeDays,
		Compress:   opts.Compress,
	}
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: currentLevel})
	defaultLogger = slog.New(handler)
	slog.SetDefault(defaultLogger)
	return w, nil
}
