package install

import "github.com/stperic/zzrouter/pkg/config"

// LoadAppsConfig loads provider config for installer plan builders that
// need to read InstallVariants from config. This is an indirection point:
// tests inject a fixture by swapping the variable (with cleanup) rather
// than touching disk, and production code calls the default loader which
// walks the XDG search path. The builtin installers call this through
// install.LoadAppsConfig so no subpackage owns a duplicate loader.
var LoadAppsConfig = func() (*config.AppsConfig, error) {
	cfg, _, err := config.LoadAppsConfigFromStandardLocations()
	return cfg, err
}
