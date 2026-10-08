package fsroot

import "errors"

// ErrInstallInProgress is raised by LockFile.Lock when another install
// for the same provider holds the per-provider on-disk lock. The root
// install package re-exports this sentinel so errors.Is matches for
// parent callers.
var ErrInstallInProgress = errors.New("install already in progress for this provider")
