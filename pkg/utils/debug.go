package utils

import (
	"fmt"
	"os"
	"sync"

	"github.com/stperic/zzrouter/pkg/observability/logger"
)

// DebugLogger provides centralized debug logging with global control
type DebugLogger struct {
	enabled bool
	mu      sync.RWMutex
}

var debugLogger = &DebugLogger{enabled: false}

// SetDebugMode enables or disables debug logging globally
func SetDebugMode(enabled bool) {
	debugLogger.mu.Lock()
	defer debugLogger.mu.Unlock()
	debugLogger.enabled = enabled
	if enabled {
		logger.SetLevel(logger.LevelDebug)
	} else {
		logger.SetLevel(logger.LevelInfo)
	}
}

// IsDebugEnabled returns the current debug state
func IsDebugEnabled() bool {
	debugLogger.mu.RLock()
	defer debugLogger.mu.RUnlock()
	return debugLogger.enabled
}

// Debugf prints a debug message if debug mode is enabled
func Debugf(format string, args ...any) {
	if IsDebugEnabled() {
		fmt.Printf("DEBUG: "+format, args...)
	}
}

// LogDebugf prints a debug message using log.Printf if debug mode is enabled
func LogDebugf(format string, args ...any) {
	if IsDebugEnabled() {
		logger.Debug(fmt.Sprintf(format, args...))
	}
}

// LogDebugln prints a debug line using log.Println if debug mode is enabled
func LogDebugln(args ...any) {
	if IsDebugEnabled() {
		logger.Debug(fmt.Sprint(args...))
	}
}

// LogInfof prints an info message using log.Printf
func LogInfof(format string, args ...any) {
	logger.Info(fmt.Sprintf(format, args...))
}

// LogWarnf prints a warning message
func LogWarnf(format string, args ...any) {
	logger.Warn(fmt.Sprintf(format, args...))
}

// LogErrorf prints an error message using log.Printf
func LogErrorf(format string, args ...any) {
	logger.Error(fmt.Sprintf(format, args...))
}

// InitDebugFromEnv initializes debug mode from environment variable
func InitDebugFromEnv() {
	if env := os.Getenv("ZZROUTER_DEBUG"); env == "true" || env == "1" {
		SetDebugMode(true)
	}
}
