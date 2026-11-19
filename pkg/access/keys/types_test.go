package keys

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestVirtualKey_IsExpired(t *testing.T) {
	t.Parallel()
	// Not expired
	future := time.Now().Add(24 * time.Hour)
	vk := &VirtualKey{ExpiresAt: &future}
	assert.False(t, vk.IsExpired())

	// Expired
	past := time.Now().Add(-24 * time.Hour)
	vk = &VirtualKey{ExpiresAt: &past}
	assert.True(t, vk.IsExpired())

	// No expiration
	vk = &VirtualKey{}
	assert.False(t, vk.IsExpired())
}

// TestValidateRefusesAnotherCallersName: an ID with ':', or the actor of
// unattributed events, could take the name of a caller that is not that app
// key, so a record holding one is refused.
func TestValidateRefusesAnotherCallersName(t *testing.T) {
	for _, id := range []string{"local:operator", "static:admin", ":", "a:", "unknown"} {
		key := VirtualKey{ID: id, Name: "Key", HashedKey: "hash", TeamID: "eng", TeamRole: "member", Role: RoleUser}
		if err := key.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("Validate with ID %q = %v, want ErrInvalid", id, err)
		}
	}
	key := VirtualKey{ID: "reader", Name: "Key", HashedKey: "hash", TeamID: "eng", TeamRole: "member", Role: RoleUser}
	if err := key.Validate(); err != nil {
		t.Errorf("Validate = %v, want nil", err)
	}
}
