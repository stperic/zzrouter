package control

import (
	"testing"

	"go.uber.org/goleak"
)

// TestLifecycle_Authenticator_NoGoroutineLeak exercises Authenticator
// Start→Stop and fails if the cache cleanup goroutine outlives the
// test. If a future edit regresses Stop to close-and-return, this
// fires.
func TestLifecycle_Authenticator_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	a := NewAuthenticator(StaticKeySet{}, nil)
	a.Start()
	a.Stop()
}
