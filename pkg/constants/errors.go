// Package constants provides application-wide constants and default values
package constants

// =============================================================================
// Error Message Constants - Centralized error message templates
// =============================================================================
// This file is the SINGLE SOURCE OF TRUTH for error message formats.
// Use these constants and helper functions for consistent error messaging.
// =============================================================================

// -----------------------------------------------------------------------------
// Not Found Error Templates
// -----------------------------------------------------------------------------

const (
	// ErrModelNotFoundFmt is the format string for model not found errors
	ErrModelNotFoundFmt = "model '%s' not found"

	// ErrInstanceNotFoundFmt is the format string for instance not found errors
	ErrInstanceNotFoundFmt = "instance '%s' not found"

	// ErrProviderNotFoundFmt is the format string for provider not found errors
	ErrProviderNotFoundFmt = "provider '%s' not found"

	// ErrNodeNotFoundFmt is the format string for host not found errors
	ErrNodeNotFoundFmt = "host '%s' not found"

	// ErrResourceNotFoundFmt is the generic format string for resource not found errors
	ErrResourceNotFoundFmt = "%s '%s' not found"
)

// -----------------------------------------------------------------------------
// Validation Error Templates
// -----------------------------------------------------------------------------

const (
	// ErrParameterRequiredFmt is the format string for missing required parameters
	ErrParameterRequiredFmt = "%s parameter is required"

	// ErrInvalidParameterFmt is the format string for invalid parameter values
	ErrInvalidParameterFmt = "invalid %s: %s"

	// ErrInvalidFormatFmt is the format string for format validation errors
	ErrInvalidFormatFmt = "invalid format for %s"
)

// -----------------------------------------------------------------------------
// Operation Error Templates
// -----------------------------------------------------------------------------

const (
	// ErrOperationFailedFmt is the format string for failed operations
	ErrOperationFailedFmt = "failed to %s: %s"

	// ErrAlreadyExistsFmt is the format string for duplicate resource errors
	ErrAlreadyExistsFmt = "%s '%s' already exists"

	// ErrNotRunningFmt is the format string for not running errors
	ErrNotRunningFmt = "%s is not running"

	// ErrAlreadyRunningFmt is the format string for already running errors
	ErrAlreadyRunningFmt = "%s is already running"
)

// -----------------------------------------------------------------------------
// Connection Error Templates
// -----------------------------------------------------------------------------

const (
	// ErrConnectionFailedFmt is the format string for connection failures
	ErrConnectionFailedFmt = "failed to connect to %s: %s"

	// ErrTimeoutFmt is the format string for timeout errors
	ErrTimeoutFmt = "operation timed out after %s"

	// ErrUnreachableFmt is the format string for unreachable errors
	ErrUnreachableFmt = "%s is unreachable"
)

// -----------------------------------------------------------------------------
// JSON Response Field Names
// -----------------------------------------------------------------------------

const (
	// JSONFieldError is the standard error field name in JSON responses
	JSONFieldError = "error"

	// JSONFieldMessage is the standard message field name in JSON responses
	JSONFieldMessage = "message"

	// JSONFieldDetails is the standard details field name in JSON responses
	JSONFieldDetails = "details"

	// JSONFieldCode is the standard code field name in JSON responses
	JSONFieldCode = "code"

	// JSONFieldStatus is the standard status field name in JSON responses
	JSONFieldStatus = "status"
)
