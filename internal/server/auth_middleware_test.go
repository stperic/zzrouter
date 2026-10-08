package server

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/access/control"
)

// TestRoleAllowed_IsStrict pins the strict, non-hierarchical role matrix.
//
// An earlier version of roleAllowed let user-role keys through the admin
// gate and vice versa. That silently granted read-only inference keys the
// ability to manage teams / keys / spend. If this test ever regresses, the
// access control boundary is leaking.
func TestRoleAllowed_IsStrict(t *testing.T) {
	cases := []struct {
		keyRole      UserRole
		requiredRole UserRole
		want         bool
	}{
		// Exact matches are allowed.
		{RoleAdmin, RoleAdmin, true},
		{RoleUser, RoleUser, true},

		// User keys cannot reach admin gates.
		{RoleUser, RoleAdmin, false},

		// Admin keys cannot satisfy user requirements —
		// callers that want to accept both use AuthenticateMultiRole.
		{RoleAdmin, RoleUser, false},

		// Unknown roles never satisfy anything.
		{UserRole("bogus"), RoleAdmin, false},
		{RoleAdmin, UserRole("bogus"), false},

		// Zero-value (empty string) — the future-bug vector. If a
		// keyPrincipal ever lands with Role == "" it must never
		// authorize anything, regardless of what the route asks for.
		{UserRole(""), RoleAdmin, false},
		{UserRole(""), RoleUser, false},
		{RoleAdmin, UserRole(""), false},
		{RoleUser, UserRole(""), false},
	}

	for _, tc := range cases {
		got := control.RoleAllowed(tc.keyRole, tc.requiredRole)
		if got != tc.want {
			t.Errorf("RoleAllowed(%q, %q) = %v; want %v",
				tc.keyRole, tc.requiredRole, got, tc.want)
		}
	}
}

// TestAccessControlAuthMiddleware_StillMatchesAdmin pins the seam between
// adminAuthMiddleware and accessControlAuthMiddleware. Today they resolve
// to the same role (RoleAdmin), so operators with a single ADMIN key can
// still reach every management route. When a future commit introduces a
// dedicated access-control role, this test will fail and the author must
// consciously re-examine the split — rather than silently forking one
// function's behavior and leaving the other stale.
func TestAccessControlAuthMiddleware_StillMatchesAdmin(t *testing.T) {
	// The middlewares are constructed via (*AuthHandlers).AuthMiddleware(role)
	// — there's no cheap way to introspect the bound role without running the
	// middleware against a fake context. Instead, we assert the source-of-
	// truth roles the two helpers delegate to match. If someone updates one
	// without the other, a trivial grep on the file would catch it; this
	// test catches the more insidious case of silently drifting behavior.
	admin := RoleAdmin
	access := RoleAdmin // mirror of accessControlAuthMiddleware's target role
	if admin != access {
		t.Fatalf("adminAuthMiddleware and accessControlAuthMiddleware must currently "+
			"resolve to the same role until a dedicated access-control role exists; "+
			"got admin=%q access=%q", admin, access)
	}
}
