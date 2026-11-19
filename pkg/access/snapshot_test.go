package access_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/stperic/zzrouter/pkg/access"
)

// A failed store admits no one: no credential or record resolves, keyless
// callers are refused and setup cannot start.
func TestFailedSnapshotAdmitsNoOne(t *testing.T) {
	t.Parallel()
	snap := access.FailedSnapshot()
	assert.True(t, snap.HasGatedTeam(), "keyless inference meets a gated team")
	assert.True(t, snap.Initialized(), "the installation reads as claimed")
	id, key := snap.ValidateRawKey("zzr_any")
	assert.Empty(t, id)
	assert.Nil(t, key)
	assert.Nil(t, snap.Key("reader"))
	assert.Nil(t, snap.Team("eng"))
	assert.Nil(t, snap.Person("person"))
	assert.Nil(t, snap.Owner())
	assert.Empty(t, snap.People())
	assert.Empty(t, snap.Invitations())
	for range snap.Keys() {
		t.Error("a failed snapshot yields a key")
	}
	for range snap.Teams() {
		t.Error("a failed snapshot yields a team")
	}
	for range snap.KeysByTeam("eng") {
		t.Error("a failed snapshot yields a team's key")
	}
	assert.False(t, (*access.Snapshot)(nil).Initialized(), "a nil snapshot still reads as empty")
}
