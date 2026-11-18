package keys

import (
	"github.com/stretchr/testify/assert"
	"testing"
	"time"
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
