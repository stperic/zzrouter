package logger

import (
	"log/slog"
	"os"
)

// Level is a logging level.
type Level = slog.Level

const (
	LevelDebug = slog.LevelDebug
	LevelInfo  = slog.LevelInfo
	LevelWarn  = slog.LevelWarn
	LevelError = slog.LevelError
)

var (
	defaultLogger *slog.Logger
	currentLevel  = new(slog.LevelVar)
)

func init() {
	// Default to Info level
	currentLevel.Set(LevelInfo)

	// Create a default handler that writes to stderr
	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: currentLevel,
	})

	defaultLogger = slog.New(handler)
	slog.SetDefault(defaultLogger)
}

// SetLevel sets the global logging level.
func SetLevel(level Level) {
	currentLevel.Set(level)
}

// Debug logs a message at debug level.
func Debug(msg string, args ...any) {
	slog.Debug(msg, args...)
}

// Info logs a message at info level.
func Info(msg string, args ...any) {
	slog.Info(msg, args...)
}

// Warn logs a message at warn level.
func Warn(msg string, args ...any) {
	slog.Warn(msg, args...)
}

// Error logs a message at error level.
func Error(msg string, args ...any) {
	slog.Error(msg, args...)
}
