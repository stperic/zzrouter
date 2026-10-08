// Package constants provides application-wide constants and default values
package constants

// =============================================================================
// Size Constants - Centralized buffer and file size configuration
// =============================================================================
// This file is the SINGLE SOURCE OF TRUTH for all size-related constants.
// Use these constants instead of hardcoding byte calculations in your code.
// =============================================================================

// -----------------------------------------------------------------------------
// Byte Size Units
// -----------------------------------------------------------------------------

const (
	// BytesPerKB is the number of bytes in a kilobyte
	BytesPerKB = 1024

	// BytesPerMB is the number of bytes in a megabyte
	BytesPerMB = 1024 * 1024

	// BytesPerGB is the number of bytes in a gigabyte
	BytesPerGB = 1024 * 1024 * 1024

	// BytesPerTB is the number of bytes in a terabyte
	BytesPerTB = 1024 * 1024 * 1024 * 1024
)

// -----------------------------------------------------------------------------
// Log File Configuration
// -----------------------------------------------------------------------------

const (
	// DefaultLogFileMaxSize is the maximum size for log files before rotation (100MB)
	DefaultLogFileMaxSize = 100 * BytesPerMB

	// DefaultLogFileBackups is the number of log file backups to keep
	DefaultLogFileBackups = 3

	// DefaultLogChannelBuffer is the default buffer size for log channels
	DefaultLogChannelBuffer = 100
)

// -----------------------------------------------------------------------------
// Sync/Transfer Limits
// -----------------------------------------------------------------------------

const (
	// SyncMaxFileSize is the maximum file size for sync operations (100GB)
	SyncMaxFileSize = 100 * BytesPerGB

	// MaxUploadSize is the maximum size for file uploads (10GB)
	MaxUploadSize = 10 * BytesPerGB
)

// -----------------------------------------------------------------------------
// Buffer Sizes
// -----------------------------------------------------------------------------

const (
	// DefaultReadBufferSize is the default buffer size for reading operations
	DefaultReadBufferSize = 32 * BytesPerKB

	// DefaultWriteBufferSize is the default buffer size for writing operations
	DefaultWriteBufferSize = 32 * BytesPerKB

	// StreamingBufferSize is the buffer size for streaming operations
	StreamingBufferSize = 4 * BytesPerKB
)

// -----------------------------------------------------------------------------
// Model Context Lengths
// -----------------------------------------------------------------------------

const (
	// DefaultContextLength is the default context length for models
	DefaultContextLength = 4096

	// SmallContextLength is for smaller/older models
	SmallContextLength = 2048

	// LargeContextLength is for larger models
	LargeContextLength = 8192

	// ExtendedContextLength is for extended context models
	ExtendedContextLength = 32768

	// MaxContextLength is the maximum supported context length
	MaxContextLength = 200000
)

// -----------------------------------------------------------------------------
// Port Ranges
// -----------------------------------------------------------------------------

const (
	// DefaultPortRangeStart is the default starting port for dynamic allocation
	DefaultPortRangeStart = 8000

	// DefaultPortRangeEnd is the default ending port for dynamic allocation
	DefaultPortRangeEnd = 8100

	// TestPortRangeStart is the starting port for test allocations
	TestPortRangeStart = 19000

	// TestPortRangeEnd is the ending port for test allocations
	TestPortRangeEnd = 19999
)
