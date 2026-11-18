package sqlstore

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/utils"
)

const (
	crashDirVar   = "SQLSTORE_CRASH_DIR"
	crashGrantVar = "SQLSTORE_CRASH_GRANT"
)

// TestCrashChild is TestKillMidCommit's child process: it claims the owner of
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
	_, err = s.People().ClaimOwner(context.Background(), os.Getenv(crashGrantVar), testProof, people.CreatePerson{Username: "owner", Name: "Owner", Password: testPassword})
	require.NoError(t, err)
	fmt.Println("claimed")
}

type claimSetup struct {
	dir, grant         string
	revision, sequence uint64
	events             int
}

// prepareClaim leaves a closed store holding a bound setup grant.
func prepareClaim(t *testing.T) claimSetup {
	t.Helper()
	c := claimSetup{dir: t.TempDir()}
	s := openStore(t, c.dir, contractOptions)
	_, err := s.CreateTeam(t.Context(), "workspace", engineering(), audit.Event{})
	require.NoError(t, err)
	c.grant = bound(t, s, people.GrantRequest{Purpose: people.PurposeSetup, TeamID: "workspace", Role: "admin"}).Token
	_, _, c.revision, c.sequence = s.Meta()
	require.NoError(t, s.Close())
	c.events = len(storedEvents(t, c.dir))
	return c
}

// startClaim runs the child and returns once its commit reaches COMMIT.
func startClaim(t *testing.T, c claimSetup) (*exec.Cmd, *bufio.Scanner) {
	t.Helper()
	child := host.Command(os.Args[0], "-test.run=^TestCrashChild$")
	child.Env = append(os.Environ(), crashDirVar+"="+c.dir, crashGrantVar+"="+c.grant)
	stdout, err := child.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, child.Start())
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	lines := bufio.NewScanner(stdout)
	for lines.Scan() && lines.Text() != "committing" {
	}
	return child, lines
}

// TestKillMidCommit kills a process while it commits an owner claim, which
// writes a person, a grant, the installation row, the audit row and the meta
// rows; the store reopens with all of it or none of it.
func TestKillMidCommit(t *testing.T) {
	t.Parallel()
	// Time one COMMIT first, so the kills spread across its syncs on any disk.
	_, lines := startClaim(t, prepareClaim(t))
	started := utils.Now()
	for lines.Scan() && lines.Text() != "claimed" {
	}
	window := 2 * time.Since(started)
	const kills = 24
	outcomes := map[bool]int{}
	for i := range kills {
		c := prepareClaim(t)
		child, _ := startClaim(t, c)
		time.Sleep(rand.N(window))
		require.NoError(t, child.Process.Kill())
		_ = child.Wait()

		reopened := openStore(t, c.dir, contractOptions)
		snap := reopened.Snapshot()
		_, _, revision, sequence := reopened.Meta()
		_, inspectErr := reopened.People().Inspect(t.Context(), c.grant)
		stored := storedEvents(t, c.dir)
		committed := snap.Initialized()
		outcomes[committed]++
		if committed {
			require.NotNil(t, snap.Owner(), "kill %d", i)
			assert.Len(t, snap.People(), 1, "kill %d", i)
			assert.Equal(t, "provider", snap.Progress().Step, "kill %d: the installation row moved with the claim", i)
			require.ErrorIs(t, inspectErr, people.ErrInvalidGrant, "kill %d: the grant was consumed", i)
			require.Len(t, stored, c.events+1, "kill %d", i)
			assert.Equal(t, audit.ActionPersonCreated, stored[c.events].Action, "kill %d", i)
			assert.Equal(t, [2]uint64{c.revision + 1, c.sequence + 1}, [2]uint64{revision, sequence}, "kill %d", i)
		} else {
			assert.Empty(t, snap.People(), "kill %d", i)
			assert.Empty(t, snap.Progress().Step, "kill %d", i)
			require.NoError(t, inspectErr, "kill %d: the grant is still usable", i)
			assert.Len(t, stored, c.events, "kill %d", i)
			assert.Equal(t, [2]uint64{c.revision, c.sequence}, [2]uint64{revision, sequence}, "kill %d", i)
		}
		require.NoError(t, reopened.Close())
	}
	t.Logf("%d kills within %v of COMMIT: %d kept the claim, %d the previous state", kills, window, outcomes[true], outcomes[false])
}
