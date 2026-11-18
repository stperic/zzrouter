package sqlstore

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
)

var updateDumps = flag.Bool("update", false, "rewrite testdata/access-vN.dump; a missing fixture database is built from the current schema")

func TestSchemaSums(t *testing.T) {
	t.Parallel()
	require.NoError(t, schema.Verify(), "a released migration changed, or SUMS was not updated")
}

// TestSchemaFixtures opens each released schema's fixture with this release and
// compares what it holds afterwards with its golden dump. Its schema must then
// equal a fresh store's, or a released migration was edited, SUMS and all.
func TestSchemaFixtures(t *testing.T) {
	latest, err := schema.Latest()
	require.NoError(t, err)
	fresh := t.TempDir()
	require.NoError(t, openStore(t, fresh, Options{}).Close())
	freshSchema := dumpSchema(t, rawDB(t, fresh))
	for version := 1; version <= latest; version++ {
		t.Run(fmt.Sprint("v", version), func(t *testing.T) {
			fixture := filepath.Join("testdata", fmt.Sprintf("access-v%d.sqlite", version))
			if _, err := os.Stat(fixture); os.IsNotExist(err) && *updateDumps && version == latest {
				buildFixture(t, fixture)
			}
			data, err := os.ReadFile(fixture)
			require.NoError(t, err, "every released schema version has a fixture")
			dir := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "access"), 0o700))
			require.NoError(t, os.WriteFile(dbPath(dir), data, 0o600))
			s := openStore(t, dir, contractOptions)
			snap := s.Snapshot()
			require.NotNil(t, snap.Key("reader"))
			require.NotNil(t, snap.Owner())
			assert.True(t, snap.HasGatedTeam())
			// The decoded records catch a codec that maps a column to another field.
			decoded, err := json.MarshalIndent(viewOf(snap), "", "  ")
			require.NoError(t, err)
			require.NoError(t, s.Close())
			db := rawDB(t, dir)
			assert.Equal(t, freshSchema, dumpSchema(t, db), "the migrated fixture has the schema a fresh store has")
			dump := dumpSchema(t, db) + dumpRows(t, db) + "decoded\n" + string(decoded) + "\n"
			golden := filepath.Join("testdata", fmt.Sprintf("access-v%d.dump", version))
			if *updateDumps {
				require.NoError(t, os.WriteFile(golden, []byte(dump), 0o600))
			}
			want, err := os.ReadFile(golden)
			require.NoError(t, err)
			assert.Equal(t, string(want), dump)
		})
	}
}

// buildFixture writes a small database through the store: a row in every
// record table and audit, with different values in same-typed neighbouring
// columns.
func buildFixture(t *testing.T, path string) {
	dir := t.TempDir()
	s := openStore(t, dir, contractOptions)
	owner := claim(t, s)
	expires, suspended := time.Date(2030, 1, 2, 3, 4, 5, 678901000, time.UTC), time.Date(2029, 6, 7, 8, 9, 10, 0, time.UTC)
	quotas := quota.QuotaConfig{RPMLimit: 1, TPMLimit: 2, MaxParallelRequests: 3, SpendLimit: 4.5, ResetPeriod: "daily", DefaultMaxTokens: 6}
	_, _, err := s.CreateKey(t.Context(), "reader", keys.VirtualKey{
		Name: "Reader", Description: "fixture key", TeamID: "eng", TeamRole: "member", Role: "user", Suspended: true, SuspendedAt: &suspended,
		SuspendedBy: "suspender", QuotaConfig: quotas, ExpiresAt: &expires, CreatedBy: "creator", UpdatedBy: "updater", Metadata: map[string]string{"env": "fixture"},
	}, audit.Event{Action: audit.ActionKeyCreated, Subject: "reader"})
	require.NoError(t, err)
	quotas.RPMLimit, quotas.ResetPeriod = 7, "weekly"
	_, err = s.CreateTeam(t.Context(), "eng", teams.Team{
		Name: "Engineering", Kind: teams.KindShared, AllowedModels: []string{"qwen"}, RouteAccess: teams.RouteAccess{Mode: "none"},
		ProviderAccess: teams.ProviderAccess{Mode: "selected", Providers: []string{"ollama"}}, SuspendedBy: "team suspender", QuotaConfig: quotas,
		CreatedBy: "team creator", UpdatedBy: "team updater", Metadata: map[string]string{"dept": "fixture"},
	}, audit.Event{})
	require.NoError(t, err)
	invitation := bound(t, s, people.GrantRequest{Purpose: people.PurposeInvitation, TeamID: "eng", Role: "user"})
	member, err := s.People().AcceptInvitation(t.Context(), invitation.Token, testProof, people.CreatePerson{Username: "member", Name: "Member", Password: testPassword})
	require.NoError(t, err)
	_, err = s.People().Update(t.Context(), member.ID, people.UpdatePerson{Suspended: new(true)})
	require.NoError(t, err)
	bound(t, s, people.GrantRequest{Purpose: people.PurposeInvitation, TeamID: "eng", Role: "admin", Issuer: people.Issuer{PersonID: owner.ID, Revision: owner.Revision, Reference: "owner"}})
	_, err = s.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeRecovery, TeamID: "eng", PersonID: member.ID, Issuer: installationIssuer})
	require.NoError(t, err)
	require.NoError(t, s.People().UpdateProgress(t.Context(), people.ProgressUpdate{Step: new("model"), Dismissed: new(true)}))
	require.NoError(t, s.People().MarkResponse(t.Context(), owner.ID))
	require.NoError(t, s.Close())
	data, err := os.ReadFile(dbPath(dir))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

// dumpSchema prints the header values and every schema object.
func dumpSchema(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	for _, pragma := range []string{"application_id", "user_version"} {
		var value int64
		require.NoError(t, db.QueryRowContext(t.Context(), "PRAGMA "+pragma).Scan(&value))
		fmt.Fprintf(&b, "%s %d\n", pragma, value)
	}
	rows, err := db.QueryContext(t.Context(), "SELECT type, name, sql FROM sqlite_schema WHERE sql IS NOT NULL ORDER BY type, name")
	require.NoError(t, err)
	for rows.Next() {
		var kind, name, text string
		require.NoError(t, rows.Scan(&kind, &name, &text))
		fmt.Fprintf(&b, "%s %s: %s\n", kind, name, text)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	return b.String()
}

// dumpRows prints every row of every table in key order.
func dumpRows(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	var tables []string
	rows, err := db.QueryContext(t.Context(), "SELECT name FROM sqlite_schema WHERE type = 'table' ORDER BY name")
	require.NoError(t, err)
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		tables = append(tables, name)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	for _, table := range tables {
		fmt.Fprintf(&b, "rows %s\n", table)
		rows, err := db.QueryContext(t.Context(), "SELECT * FROM "+table+" ORDER BY 1") //nolint:gosec // names read from sqlite_schema of a test database
		require.NoError(t, err)
		columns, err := rows.Columns()
		require.NoError(t, err)
		for rows.Next() {
			values := make([]any, len(columns))
			targets := make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			require.NoError(t, rows.Scan(targets...))
			fields := make([]string, len(columns))
			for i, value := range values {
				fields[i] = fmt.Sprintf("%s=%#v", columns[i], value)
			}
			fmt.Fprintf(&b, "  %s\n", strings.Join(fields, " "))
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
	}
	return b.String()
}
