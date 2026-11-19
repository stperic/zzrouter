package sqlstore

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/utils"
)

const (
	crashDirVar = "SQLSTORE_CRASH_DIR"
	crashOpVar  = "SQLSTORE_CRASH_OP"
	crashArgVar = "SQLSTORE_CRASH_ARG"
)

// crashOp is one commit TestKillMidCommit kills: prepare leaves the store it
// starts from and returns run's argument; done and none hold when the store
// reopens with all of the commit, or with none of it.
type crashOp struct {
	prepare    func(t *testing.T, s *Store) string
	run        func(s *Store, arg string) error
	done, none func(t *testing.T, s *Store, arg string) bool
	events     int
}

var soloEvent = audit.Event{Action: audit.ActionKeyCreated, Actor: "operator", Subject: "solo"}

var crashOps = map[string]crashOp{
	// An owner claim writes a person, a grant, the installation row, the audit row and the meta rows.
	"claim owner": {
		prepare: func(t *testing.T, s *Store) string {
			_, err := s.CreateTeam(t.Context(), "workspace", engineering(), audit.Event{})
			require.NoError(t, err)
			return bound(t, s, people.GrantRequest{Purpose: people.PurposeSetup, TeamID: "workspace", Role: "admin"}).Token
		},
		run: func(s *Store, grant string) error {
			_, err := s.People().ClaimOwner(context.Background(), grant, testProof, people.CreatePerson{Username: "owner", Name: "Owner", Password: testPassword})
			return err
		},
		done: func(t *testing.T, s *Store, grant string) bool {
			snap := s.Snapshot()
			_, err := s.People().Inspect(t.Context(), grant)
			return snap.Owner() != nil && len(snap.People()) == 1 && snap.Progress().Step == "provider" && err != nil
		},
		none: func(t *testing.T, s *Store, grant string) bool {
			snap := s.Snapshot()
			_, err := s.People().Inspect(t.Context(), grant)
			return !snap.Initialized() && len(snap.People()) == 0 && snap.Progress().Step == "" && err == nil
		},
		events: 1,
	},
	"create a key with its personal team": {
		prepare: func(*testing.T, *Store) string { return "" },
		run: func(s *Store, _ string) error {
			_, _, err := s.CreateKey(context.Background(), "solo", readerKey(), soloEvent)
			return err
		},
		done: func(_ *testing.T, s *Store, _ string) bool {
			return s.Snapshot().Key("solo") != nil && s.Snapshot().Team(teams.PersonalID("solo")) != nil
		},
		none: func(_ *testing.T, s *Store, _ string) bool {
			return s.Snapshot().Key("solo") == nil && s.Snapshot().Team(teams.PersonalID("solo")) == nil
		},
		events: 2,
	},
	"delete a key with its personal team": {
		prepare: func(t *testing.T, s *Store) string {
			_, _, err := s.CreateKey(t.Context(), "solo", readerKey(), soloEvent)
			require.NoError(t, err)
			return ""
		},
		run: func(s *Store, _ string) error { return s.DeleteKey(context.Background(), "solo", soloEvent) },
		done: func(_ *testing.T, s *Store, _ string) bool {
			return s.Snapshot().Key("solo") == nil && s.Snapshot().Team(teams.PersonalID("solo")) == nil
		},
		none: func(_ *testing.T, s *Store, _ string) bool {
			return s.Snapshot().Key("solo") != nil && s.Snapshot().Team(teams.PersonalID("solo")) != nil
		},
		events: 2,
	},
	"issue the setup grant with its workspace": {
		prepare: func(*testing.T, *Store) string { return "" },
		run: func(s *Store, _ string) error {
			_, err := s.IssueSetup(context.Background(), installationIssuer)
			return err
		},
		done: func(_ *testing.T, s *Store, _ string) bool {
			workspaces := maps.Collect(s.Snapshot().Teams())
			grants := s.Snapshot().PeopleState().Records().Grants
			for _, grant := range grants {
				return len(workspaces) == 1 && len(grants) == 1 && workspaces[grant.TeamID] != nil
			}
			return false
		},
		none: func(_ *testing.T, s *Store, _ string) bool {
			return len(maps.Collect(s.Snapshot().Teams())) == 0 && len(s.Snapshot().PeopleState().Records().Grants) == 0
		},
		events: 1,
	},
}

// TestCrashChild is TestKillMidCommit's child process: it runs one crash op on
// the store it is given and prints "committing" when its commit reaches COMMIT.
func TestCrashChild(t *testing.T) {
	dir := os.Getenv(crashDirVar)
	if dir == "" {
		return
	}
	s, err := Open(context.Background(), dir, contractOptions)
	require.NoError(t, err)
	s.beforeCommit = func(*sql.Tx) error {
		_, err := fmt.Println("committing")
		return err
	}
	require.NoError(t, crashOps[os.Getenv(crashOpVar)].run(s, os.Getenv(crashArgVar)))
	fmt.Println("committed")
}

type crashSetup struct {
	dir, arg           string
	revision, sequence uint64
	events             int
}

// prepareCrash leaves a closed store ready for op.
func prepareCrash(t *testing.T, op crashOp) crashSetup {
	t.Helper()
	c := crashSetup{dir: t.TempDir()}
	s := openStore(t, c.dir, contractOptions)
	c.arg = op.prepare(t, s)
	c.revision, c.sequence = versions(s)
	require.NoError(t, s.Close())
	c.events = len(storedEvents(t, c.dir))
	return c
}

// startCrash runs the child and returns once its commit reaches COMMIT.
func startCrash(t *testing.T, name string, c crashSetup) (*exec.Cmd, *bufio.Scanner) {
	t.Helper()
	child := host.Command(os.Args[0], "-test.run=^TestCrashChild$")
	child.Env = append(os.Environ(), crashDirVar+"="+c.dir, crashOpVar+"="+name, crashArgVar+"="+c.arg)
	stdout, err := child.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, child.Start())
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	lines := bufio.NewScanner(stdout)
	for lines.Scan() && lines.Text() != "committing" {
	}
	return child, lines
}

// TestKillMidCommit kills a process while it commits each crash op; the store
// reopens with all of the commit or none of it, its audit rows and meta included.
func TestKillMidCommit(t *testing.T) {
	t.Parallel()
	for name, op := range crashOps {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// Time one COMMIT first, so the kills spread across its syncs on any disk.
			_, lines := startCrash(t, name, prepareCrash(t, op))
			started := utils.Now()
			for lines.Scan() && lines.Text() != "committed" {
			}
			window := 2 * time.Since(started)
			const kills = 24
			outcomes := map[bool]int{}
			for i := range kills {
				c := prepareCrash(t, op)
				child, _ := startCrash(t, name, c)
				time.Sleep(rand.N(window))
				require.NoError(t, child.Process.Kill())
				_ = child.Wait()

				reopened := openStore(t, c.dir, contractOptions)
				revision, sequence := versions(reopened)
				stored := len(storedEvents(t, c.dir))
				committed := op.done(t, reopened, c.arg)
				outcomes[committed]++
				if committed {
					assert.Equal(t, c.events+op.events, stored, "kill %d: the audit rows moved with the change", i)
					assert.Equal(t, [2]uint64{c.revision + 1, c.sequence + 1}, [2]uint64{revision, sequence}, "kill %d", i)
				} else {
					require.True(t, op.none(t, reopened, c.arg), "kill %d: the store holds part of the commit", i)
					assert.Equal(t, c.events, stored, "kill %d", i)
					assert.Equal(t, [2]uint64{c.revision, c.sequence}, [2]uint64{revision, sequence}, "kill %d", i)
				}
				require.NoError(t, reopened.Close())
			}
			t.Logf("%d kills within %v of COMMIT: %d kept the commit, %d the previous state", kills, window, outcomes[true], outcomes[false])
		})
	}
}
