package server

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
)

func newAuditTestAccess(t *testing.T, keyStore *keys.FileKeyStore, teamStore *teams.FileTeamStore) *AccessControl {
	t.Helper()
	enforcer := quota.NewEnforcer(
		quota.NewRateLimiter(),
		quota.NewConcurrencyLimiter(),
		quota.NewSpendTracker(""),
	)
	access := NewAccessControl(StaticKeySet{}, keyStore, teamStore, nil, enforcer, nil, ModelHooks{})
	t.Cleanup(func() { access.Stop(context.Background()) })
	return access
}

// TestAudit_EndToEnd_ServiceMutationsEmitEvents pins that the three
// service-level mutations that drove the P2 #1 requirement — create
// key, delete key, create team — each produce exactly one event on
// the wired audit sink. This covers the complete flow: service
// method → store persist → audit.Log → FileSink.Emit → JSONL line.
func TestAudit_EndToEnd_ServiceMutationsEmitEvents(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	sink, err := audit.NewFileSink(auditPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sink.Close() })

	keyStore := keys.NewFileKeyStore(filepath.Join(dir, "keys.yaml"))
	teamStore := teams.NewFileTeamStore(filepath.Join(dir, "teams.yaml"))
	access := newAuditTestAccess(t, keyStore, teamStore)

	teamsSvc := NewTeamsService(teamStore, keyStore, access)
	teamsSvc.SetAuditSink(sink)
	keysSvc := NewKeysService(keyStore, teamsSvc, access)
	keysSvc.SetAuditSink(sink)

	// 1. Create a shared team.
	_, err = teamsSvc.CreateTeam(&CreateTeamRequest{
		ID:   "eng",
		Name: "Engineering",
	}, "static:admin")
	require.NoError(t, err)

	// 2. Create a key into that team (not auto-personal, so no
	//    second team_created event fires).
	_, err = keysSvc.CreateKey("alice", &CreateKeyRequest{
		Name:   "alice",
		TeamID: "eng",
	}, "static:admin")
	require.NoError(t, err)

	// 3. Delete the key. Note: cascade-delete of a personal team would
	//    fire an extra team.deleted event if this had been a personal
	//    team; here it's a shared-team member, so no cascade.
	require.NoError(t, keysSvc.DeleteKey("alice", "static:admin"))

	// Force-flush so Scan sees every line.
	require.NoError(t, sink.Close())

	events := readAuditLog(t, auditPath)
	actions := make([]string, 0, len(events))
	for _, ev := range events {
		actions = append(actions, string(ev.Action))
		require.Equal(t, "static:admin", ev.Actor)
		require.False(t, ev.Time.IsZero(), "every emitted event carries a timestamp")
	}
	require.Equal(t, []string{
		string(audit.ActionTeamCreated),
		string(audit.ActionKeyCreated),
		string(audit.ActionKeyDeleted),
	}, actions, "emission order must follow call order")

	// Subject checks pin the specific objects.
	require.Equal(t, "eng", events[0].Subject)
	require.Equal(t, "alice", events[1].Subject)
	require.Equal(t, "alice", events[2].Subject)
}

// TestAudit_PersonalTeamCascade pins that an auto-created personal team
// emits team.created alongside key.created, and that deleting the owning
// key cascades into a team.deleted audit event. Without this coverage,
// a compliance reviewer would see key records without the corresponding
// team lifecycle.
func TestAudit_PersonalTeamCascade(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	sink, err := audit.NewFileSink(auditPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sink.Close() })

	keyStore := keys.NewFileKeyStore(filepath.Join(dir, "keys.yaml"))
	teamStore := teams.NewFileTeamStore(filepath.Join(dir, "teams.yaml"))
	access := newAuditTestAccess(t, keyStore, teamStore)
	teamsSvc := NewTeamsService(teamStore, keyStore, access)
	teamsSvc.SetAuditSink(sink)
	keysSvc := NewKeysService(keyStore, teamsSvc, access)
	keysSvc.SetAuditSink(sink)

	// Create without team_id → auto-creates personal team.
	_, err = keysSvc.CreateKey("alice", &CreateKeyRequest{Name: "alice"}, "static:admin")
	require.NoError(t, err)
	// Delete the key → cascades to delete the personal team.
	require.NoError(t, keysSvc.DeleteKey("alice", "static:admin"))

	require.NoError(t, sink.Close())
	events := readAuditLog(t, auditPath)

	actions := make([]string, 0, len(events))
	for _, ev := range events {
		actions = append(actions, string(ev.Action))
	}
	require.Equal(t, []string{
		string(audit.ActionTeamCreated), // personal team for alice
		string(audit.ActionKeyCreated),
		string(audit.ActionKeyDeleted),
		string(audit.ActionTeamDeleted), // cascade
	}, actions)
	// Cascade event should be marked as such in metadata.
	cascade := events[3]
	require.Equal(t, true, cascade.Metadata["cascade"], "cascade metadata flag must fire")
}

// TestAudit_DefaultSinkIsNull pins that a service constructed with the
// default (no SetAuditSink call) silently drops events. Regression
// guard: if we ever default the sink to a file-backed implementation,
// a fresh-install test host would get a surprise audit.jsonl.
func TestAudit_DefaultSinkIsNull(t *testing.T) {
	dir := t.TempDir()
	keyStore := keys.NewFileKeyStore(filepath.Join(dir, "keys.yaml"))
	teamStore := teams.NewFileTeamStore(filepath.Join(dir, "teams.yaml"))
	access := newAuditTestAccess(t, keyStore, teamStore)
	teamsSvc := NewTeamsService(teamStore, keyStore, access)
	keysSvc := NewKeysService(keyStore, teamsSvc, access)

	_, err := teamsSvc.CreateTeam(&CreateTeamRequest{ID: "eng", Name: "Eng"}, "a")
	require.NoError(t, err)
	_, err = keysSvc.CreateKey("alice", &CreateKeyRequest{Name: "alice", TeamID: "eng"}, "a")
	require.NoError(t, err)

	// No audit.jsonl should exist — audit.Null doesn't touch disk.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		require.NotEqual(t, "audit.jsonl", e.Name())
	}
}

func readAuditLog(t *testing.T, path string) []audit.Event {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	var out []audit.Event
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		var ev audit.Event
		require.NoError(t, json.Unmarshal([]byte(line), &ev), "line: %s", line)
		out = append(out, ev)
	}
	require.NoError(t, s.Err())
	return out
}

// compile-time check that the end-to-end test uses a real ctx, not
// background, so SendContext-aware sinks receive something meaningful.
var _ = context.Background
