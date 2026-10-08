package prov_apps

import (
	"errors"

	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/port"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
)

// Re-exports from subpackages. Subpackage is source of truth; the
// re-export keeps the root-level error surface stable for consumers
// (e.g., internal/server) that use errors.Is against prov_apps.ErrX
// regardless of where the error was raised. Re-exports are value
// aliases — never errors.New with duplicated text, which would break
// errors.Is across the package boundary.
var (
	ErrInstanceNotFound      = instance.ErrInstanceNotFound
	ErrInstanceAlreadyExists = instance.ErrInstanceAlreadyExists
	ErrModelAlreadyLoaded    = instance.ErrModelAlreadyLoaded
	ErrAtCapacity            = instance.ErrAtCapacity
	ErrInstallInProgress     = install.ErrInstallInProgress
	ErrUnsupportedPlatform   = install.ErrUnsupportedPlatform
	ErrPortConflict          = port.ErrPortConflict

	// Raised while building or validating a launch. They reach
	// internal/server through LaunchInstance, which is why they are
	// re-exported here: a caller asking for a model that is not on
	// this node, or whose local copy is half-downloaded, has made a
	// mistake the HTTP layer must report as theirs.
	ErrModelNotLocal   = process.ErrModelNotLocal
	ErrModelIncomplete = process.ErrModelIncomplete
	ErrDangerousEnvVar = process.ErrDangerousEnvVar
)

// Locally-declared sentinels raised from code that lives in this package
// (manager_lifecycle.go, install_coordinator.go, manager_init.go).
var (
	// ErrProviderNotFound is returned when a requested provider does not exist.
	ErrProviderNotFound = errors.New("provider not found")

	// ErrPortUnavailable is returned when no ports are available for allocation.
	ErrPortUnavailable = errors.New("no available ports")

	// ErrPortOutOfRange is returned when a requested port is outside the configured range.
	ErrPortOutOfRange = errors.New("port outside configured range")

	// ErrShutdown is returned when an operation is attempted after shutdown.
	ErrShutdown = errors.New("manager is shutting down")

	// ErrInstancesRunning is returned when an upgrade is attempted while instances are active.
	ErrInstancesRunning = errors.New("instances still running for this provider")

	// ErrProviderNotManaged is returned when upgrade/uninstall is attempted on a provider
	// that was not installed by zzRouter (e.g., installed via Homebrew, pip, or system package).
	ErrProviderNotManaged = errors.New("provider is installed but not managed by zzRouter")

	// ErrProviderAlreadyInstalled is returned when install is attempted on a provider
	// that is already installed by zzRouter. Callers should use upgrade or uninstall instead.
	ErrProviderAlreadyInstalled = errors.New("provider is already installed; use upgrade or uninstall first")

	// ErrParameterValidation is returned when LaunchRequest.Parameters fail
	// the per-provider validation schema. The wrapped raise site preserves
	// the inner validator error for context via fmt.Errorf("%w: %w", ...).
	ErrParameterValidation = errors.New("parameter validation failed")
)
