package install

import (
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/preflight"
)

// Thin re-exports of fsroot symbols used by external callers
// (internal/server, pkg/prov_apps). New callers should depend on fsroot
// directly.
var (
	SetProviderRootOverride = fsroot.SetProviderRootOverride
	SetHardenVenvDefault    = fsroot.SetHardenVenvDefault
	SetExecEnv              = fsroot.SetExecEnv
	BuildProxyEnv           = fsroot.BuildProxyEnv
	ReadInstalledVersion    = fsroot.ReadInstalledVersion
	ProviderBinDir          = fsroot.ProviderBinDir
	ProviderVenvDir         = fsroot.ProviderVenvDir
)

// PreflightReport is aliased so the HTTP service layer
// (internal/server/providers_service.go) keeps receiving the install.*
// surface unchanged. Marshals identically via the alias.
type PreflightReport = preflight.Report
