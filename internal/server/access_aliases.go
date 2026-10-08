// package server — permanent type/const re-exports from pkg/access/control.
//
// These are zero-cost Go type aliases: `UserRole = control.UserRole` is
// literally the same type at compile time. The server package re-exports
// them for call-site brevity in handlers, middleware, and tests that
// would otherwise read `control.UserRole`/`control.RoleAdmin` at every
// reference. Deleting them would force ~30 mechanical call-site
// rewrites across auth_middleware, access_control, and tests for no
// readability gain.
//
// Cleanup trigger: delete the aliases and inline the control.* names
// when any non-test caller outside internal/server imports
// pkg/access/control directly. Verify with:
//
//	grep -rl '"github.com/stperic/zzrouter/pkg/access/control"' \
//	    --include='*.go' . | grep -v '_test\.go' | grep -v internal/server
//
// Empty output today — shim is still doing its job.

package server

import (
	"github.com/stperic/zzrouter/pkg/access/control"
)

// ===== Pure data-type re-exports =====

type (
	AccessContext = control.AccessContext
	KeyPrincipal  = control.KeyPrincipal
	TeamPrincipal = control.TeamPrincipal

	StaticKeySet = control.StaticKeySet
	UserRole     = control.UserRole
)

// ===== Role constants =====

const (
	RoleAdmin = control.RoleAdmin
	RoleUser  = control.RoleUser
)
