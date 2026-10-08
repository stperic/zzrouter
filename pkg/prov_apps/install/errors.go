package install

import (
	"errors"

	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/preflight"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/variant"
)

// Package-level sentinels for discriminable failure modes during install
// planning and execution. Callers branch on these via errors.Is — the
// wrapped forms used at raise sites still satisfy Is semantics while
// carrying per-call context (platform, filename, etc.).
//
// ErrInstallInProgress, ErrNoWritePermission, and the three ErrNo*Variant
// sentinels all live with their raise sites (fsroot lockfile, preflight
// WritePermission, variant selector). Root re-exports them via var aliases
// so errors.Is keeps matching for callers that imported install before
// the respective splits.
var (
	ErrInstallInProgress   = fsroot.ErrInstallInProgress
	ErrUnsupportedPlatform = errors.New("provider not supported on target platform")
	ErrNoWritePermission   = preflight.ErrNoWritePermission
	ErrNoVariantsDeclared  = variant.ErrNoVariantsDeclared
	ErrNoMatchingVariant   = variant.ErrNoMatchingVariant
	ErrNoGPUMatch          = variant.ErrNoGPUMatch
)
