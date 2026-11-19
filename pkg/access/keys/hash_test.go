package keys

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateRawKey(t *testing.T) {
	t.Parallel()
	key, err := GenerateRawKey()
	require.NoError(t, err)
	assert.True(t, len(key) > len(KeyPrefix))
	assert.Equal(t, KeyPrefix, key[:len(KeyPrefix)])

	// Keys should be unique
	key2, err := GenerateRawKey()
	require.NoError(t, err)
	assert.NotEqual(t, key, key2)
}

func TestHashAndVerify(t *testing.T) {
	t.Parallel()
	rawKey := "zzr_test-key-for-hashing-purposes"

	hash, err := HashKey(rawKey)
	require.NoError(t, err)
	assert.Contains(t, hash, "$argon2id$")

	assert.True(t, VerifyKey(rawKey, hash))
	assert.False(t, VerifyKey("wrong-key", hash))
	assert.False(t, VerifyKey("", hash))
}

func TestVerifyKey_InvalidHash(t *testing.T) {
	t.Parallel()
	assert.False(t, VerifyKey("key", "not-a-hash"))
	assert.False(t, VerifyKey("key", "$argon2id$v=19$m=65536,t=1,p=2$bad"))
	assert.False(t, VerifyKey("key", ""))
}
