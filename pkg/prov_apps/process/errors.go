package process

import "errors"

// ErrDangerousEnvVar is returned by the launcher when a caller-supplied
// environment variable matches the dangerous-name blocklist (LD_PRELOAD,
// DYLD_*, PYTHONPATH when set to write-enabled dirs, etc.). The wrapped
// raise site includes the offending key name.
var ErrDangerousEnvVar = errors.New("dangerous environment variable rejected")
