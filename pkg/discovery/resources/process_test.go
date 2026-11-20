package resources

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessTreeSumsAndAgreesWithGPUAncestry(t *testing.T) {
	cmd := host.Command(os.Args[0], "-test.run=^TestResourceProcessHelper$", "--", "root")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		assert.NoError(t, cmd.Wait())
	})
	lines := bufio.NewScanner(stdout)
	require.True(t, lines.Scan())
	pid, err := strconv.ParseInt(lines.Text(), 10, 32)
	child := int32(pid)
	require.NoError(t, err)
	root := cmd.Process.Pid
	parents, err := processParents(t.Context())
	if errors.Is(err, syscall.EPERM) {
		t.Skipf("sandbox denied process enumeration: %v", err)
	}
	require.NoError(t, err)
	list := func(context.Context) (map[int32]int32, error) { return parents, nil }
	parentOf := func(pid int32) (int32, error) { return parents[pid], nil }
	requireOwner := func(tree map[int32]processSample, want int, isRoot func(int) bool) {
		t.Helper()
		for pid := range tree {
			owner, _ := OwningPID(int(pid), isRoot, parentOf)
			require.Equal(t, want, owner, "owner of PID %d", pid)
		}
	}
	rootOnly := func(pid int) bool { return pid == root }
	pools := []Pool{{Kind: GPU, Holders: []Holder{{PID: int(child), MemoryMiB: new(int64(7))}}}}
	owners := []Run{{ID: "root", PID: root}, {ID: "shared", PID: root}}
	runs, trees := sampleRuns(t.Context(), owners, pools, true, list)
	tree := trees["root"]
	require.Contains(t, tree, int32(root))
	require.Contains(t, tree, child)
	requireOwner(tree, root, rootOnly)
	var total uint64
	for pid, sample := range tree {
		require.NotNil(t, sample.memory, "RSS of PID %d", pid)
		total += *sample.memory
	}
	want := int64(total >> 20)
	require.Positive(t, want)
	require.Equal(t, &want, runs["root"].HostMemoryMiB)
	require.Nil(t, runs["root"].CPUPct)
	require.Equal(t, &want, runs["shared"].HostMemoryMiB)
	require.Equal(t, "root", pools[0].Holders[0].RunID)

	unifiedRuns, unifiedTrees := sampleRuns(t.Context(), owners, nil, false, list)
	require.Nil(t, unifiedRuns["root"].HostMemoryMiB)
	require.Contains(t, unifiedTrees["root"], int32(root))
	require.NotNil(t, unifiedTrees["root"][child].cpuTime)
	requireOwner(unifiedTrees["root"], root, rootOnly)

	owners = append(owners, Run{ID: "nested", PID: int(child)})
	runs, trees = sampleRuns(t.Context(), owners, pools, true, list)
	require.Equal(t, "nested", pools[0].Holders[0].RunID)
	require.Contains(t, trees["root"], int32(root))
	require.Contains(t, trees["nested"], child)
	eitherRoot := func(pid int) bool { return pid == root || pid == int(child) }
	requireOwner(trees["root"], root, eitherRoot)
	requireOwner(trees["nested"], int(child), eitherRoot)
	require.Positive(t, *runs["nested"].HostMemoryMiB)
}

func TestResourceProcessHelper(t *testing.T) {
	switch os.Args[len(os.Args)-1] {
	case "root":
		cmd := host.CommandContext(t.Context(), os.Args[0], "-test.run=^TestResourceProcessHelper$", "--", "leaf")
		cmd.Stdout = os.Stdout
		in, err := cmd.StdinPipe()
		require.NoError(t, err)
		require.NoError(t, cmd.Start())
		_, _ = io.Copy(io.Discard, os.Stdin)
		require.NoError(t, in.Close())
		require.NoError(t, cmd.Wait())
	case "leaf":
		_, _ = fmt.Fprintln(os.Stdout, os.Getpid())
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
}

func TestSnapshotCPUPercentCountsEachCoreAndLeavesMissingTimeUnknown(t *testing.T) {
	before := Snapshot{
		hostAt: time.Unix(1, 0), processAt: time.Unix(1, 0), hostCPU: &cpu.TimesStat{User: 10, System: 5, Idle: 20, Iowait: 3},
		processes: map[string]map[int32]processSample{"run": {
			1: {cpuTime: new(1.25), started: 1},
			2: {cpuTime: new(1.50), started: 2},
		}},
	}
	current := Snapshot{
		hostAt: before.hostAt.Add(time.Second), processAt: before.processAt.Add(time.Second), hostCPU: &cpu.TimesStat{User: 11, System: 6, Idle: 21, Iowait: 4},
		Pools: []Pool{{Kind: Host}}, Runs: map[string]RunSample{"run": {}},
		processes: map[string]map[int32]processSample{"run": {
			1: {cpuTime: new(2.0), started: 1},
			2: {cpuTime: new(2.25), started: 2},
		}},
	}
	current.applyCPU(&before)
	assert.Equal(t, new(200.0), current.Pools[0].CPUPct)
	assert.Equal(t, new(150.0), current.Runs["run"].CPUPct)
	assert.Equal(t, new(int64(12)), residentMiB(map[int32]processSample{
		1: {memory: new(uint64(9 << 20))}, 2: {memory: new(uint64(3 << 20))},
	}))
	assert.Nil(t, residentMiB(map[int32]processSample{1: {memory: new(uint64(9 << 20))}, 2: {}}))
	assert.Nil(t, cpuDelta(current.processes["run"], nil, before.processAt.UnixMilli()))
	previous := before.processes["run"]
	previous[2] = processSample{cpuTime: new(1.5), started: 9}
	assert.Nil(t, cpuDelta(current.processes["run"], previous, before.processAt.UnixMilli()), "a reused PID is a new process")
}

func TestPressureLevelsAndUnifiedPoolShape(t *testing.T) {
	for _, value := range []uint32{1, 2, 4} {
		assert.Equal(t, new(int(value)), pressure(value))
	}
	for _, value := range []uint32{0, 3, 5} {
		assert.Nil(t, pressure(value))
	}
	snapshot := sample(t.Context(), nil, nil, true, false)
	require.Len(t, snapshot.Pools, 1)
	assert.Equal(t, Unified, snapshot.Pools[0].Kind)
	assert.Equal(t, "unified", snapshot.Pools[0].ID)
	require.NotNil(t, snapshot.Pools[0].AvailableMiB)
	assert.Empty(t, snapshot.Pools[0].Holders)
}

func TestNativeRAMSnapshotDoesNotReportDerivedUsage(t *testing.T) {
	pool, _ := systemPool(t.Context(), false)
	total, used, available := pool.MemoryBytes()
	require.NotNil(t, total)
	require.NotNil(t, available)
	assert.Nil(t, used)
	assert.Nil(t, pool.UsedMiB)
}

func TestProcessTreeSkipsChildThatExitsAfterListing(t *testing.T) {
	cmd := host.Command(os.Args[0], "-test.run=^TestResourceProcessHelper$", "--", "leaf")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	root, child := int32(os.Getpid()), int32(cmd.Process.Pid)
	children := map[int32][]int32{root: {child}}
	tree := processTree(t.Context(), root, nil, children, true, func(ctx context.Context, pid int32, host bool) (processSample, bool) {
		if pid == child {
			require.NoError(t, stdin.Close())
			require.NoError(t, cmd.Wait())
		}
		return readProcess(ctx, pid, host)
	})
	require.Len(t, tree, 1)
	require.NotContains(t, tree, child)
	require.NotNil(t, residentMiB(tree))
}

func TestProcessTreeUnreadableLiveChildMakesUsageUnknown(t *testing.T) {
	tree := processTree(t.Context(), 1, nil, map[int32][]int32{1: {2}}, true, func(_ context.Context, pid int32, _ bool) (processSample, bool) {
		if pid == 2 {
			return processSample{}, true
		}
		return processSample{memory: new(uint64(4 << 20))}, true
	})
	require.Len(t, tree, 2)
	require.Nil(t, residentMiB(tree))
}

func TestProcessCPUCountsForksAndIgnoresExitedChildren(t *testing.T) {
	const fromMillis = 1000
	previous := map[int32]processSample{
		1: {started: 10, cpuTime: new(1.0)},
		2: {started: 20, cpuTime: new(10.0)},
	}
	current := map[int32]processSample{
		1: {started: 10, cpuTime: new(1.5)},
		3: {started: 1200, cpuTime: new(0.25)},
		4: {started: 1300, cpuTime: new(0.5)},
	}
	require.Equal(t, new(1.25), cpuDelta(current, previous, fromMillis))
	current[2] = processSample{started: 1100, cpuTime: new(0.25)}
	require.Equal(t, new(1.5), cpuDelta(current, previous, fromMillis), "a reused PID has its own lifetime")
}

func TestGPUHolderAttributionSurvivesFailedProcessListing(t *testing.T) {
	cmd := host.Command(os.Args[0], "-test.run=^TestResourceProcessHelper$", "--", "leaf")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		assert.NoError(t, cmd.Wait())
	})
	root := os.Getpid()
	pools := []Pool{{Kind: GPU, Holders: []Holder{{PID: root}, {PID: cmd.Process.Pid}}}}
	runs, trees := sampleRuns(t.Context(), []Run{{ID: "engine", PID: root}}, pools, true, func(context.Context) (map[int32]int32, error) {
		return nil, os.ErrPermission
	})
	require.Nil(t, runs)
	require.Nil(t, trees)
	require.Equal(t, "engine", pools[0].Holders[0].RunID)
	require.Equal(t, "engine", pools[0].Holders[1].RunID)
}

func TestWindowsProcessParentsRejectReusedParentPID(t *testing.T) {
	parents := verifyProcessParents(
		map[int32]int32{1: 0, 2: 1, 3: 1, 4: 1, 5: 9, 6: 1, 7: 8},
		map[int32]int64{1: 200, 2: 100, 3: 201, 4: 200, 5: 400, 7: 300},
	)
	require.Equal(t, map[int32]int32{1: 0, 2: 0, 3: 1, 4: 1, 5: 0, 6: 0, 7: 0}, parents)
}
