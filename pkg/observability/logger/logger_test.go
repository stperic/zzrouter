package logger

import (
	"log/slog"
	"testing"
)

func TestLevelConstants(t *testing.T) {
	// Verify level constants match slog levels
	if LevelDebug != slog.LevelDebug {
		t.Errorf("LevelDebug mismatch: got %v, want %v", LevelDebug, slog.LevelDebug)
	}
	if LevelInfo != slog.LevelInfo {
		t.Errorf("LevelInfo mismatch: got %v, want %v", LevelInfo, slog.LevelInfo)
	}
	if LevelWarn != slog.LevelWarn {
		t.Errorf("LevelWarn mismatch: got %v, want %v", LevelWarn, slog.LevelWarn)
	}
	if LevelError != slog.LevelError {
		t.Errorf("LevelError mismatch: got %v, want %v", LevelError, slog.LevelError)
	}
}
