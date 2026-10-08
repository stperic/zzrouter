package install

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/jobs"
)

// recordingHandle counts what actually reaches a stream subscriber.
type recordingHandle struct {
	jobs.Handle
	progress []int
}

func (h *recordingHandle) Progress(pct int, _ string, _ jobs.Bytes) {
	h.progress = append(h.progress, pct)
}
func (h *recordingHandle) Meta(jobs.Meta) {}
func (h *recordingHandle) ID() string     { return "test-job" }

// A download reports byte progress once per read of the HTTP body. One
// install put ~49 000 frames on its job stream that way, carrying 77
// distinct percent values. The state has to stay exact; only the
// fan-out is bounded.
func TestDownloadProgressCoalescesRepeatedPercent(t *testing.T) {
	h := &recordingHandle{}
	p := &InstallProgress{}
	p.SetJobHandle(h)

	const total = int64(10000)
	// 500 reads that span only 5 distinct percentages.
	for i := 1; i <= 500; i++ {
		done := int64(i) * total / 500 / 20 // stays within 0-5%
		p.SetDownloadProgress(done, total, int(done*100/total))
	}

	assert.Less(t, len(h.progress), 20,
		"500 reads over ~5 percentages must not become 500 frames")
	require.NotEmpty(t, h.progress, "a subscriber still has to see progress")

	// State is untouched by the coalescing: a REST poller sees the real
	// byte count, not the last published one.
	snap := p.Snapshot()
	assert.Equal(t, int64(500)*total/500/20, snap.BytesDone)
	assert.Equal(t, total, snap.BytesTotal)
}

// Every percentage the download actually reaches must still be
// published — coalescing may drop repeats, never transitions.
func TestDownloadProgressKeepsEveryPercentTransition(t *testing.T) {
	h := &recordingHandle{}
	p := &InstallProgress{}
	p.SetJobHandle(h)

	const total = int64(1000)
	for done := int64(0); done <= total; done++ {
		p.SetDownloadProgress(done, total, int(done*100/total))
	}

	assert.Len(t, h.progress, 101, "0..100 inclusive, each exactly once")
	assert.Equal(t, 0, h.progress[0])
	assert.Equal(t, 100, h.progress[len(h.progress)-1],
		"the terminal percentage must reach the subscriber")
}

// A download stuck at one percentage still has to prove it is alive.
func TestDownloadProgressTicksWhileStalled(t *testing.T) {
	h := &recordingHandle{}
	p := &InstallProgress{}
	p.SetJobHandle(h)

	p.SetDownloadProgress(50, 1000, 5)
	require.Len(t, h.progress, 1)

	// Same percentage, but long enough ago that a subscriber would
	// otherwise be staring at a silent stream.
	p.mu.Lock()
	p.lastEmitAt = p.lastEmitAt.Add(-2 * downloadProgressMinInterval)
	p.mu.Unlock()

	p.SetDownloadProgress(51, 1000, 5)
	assert.Len(t, h.progress, 2, "a stalled download must still tick")
}

// A new step is a new download; the previous one's last published
// percentage must not swallow its first update.
func TestDownloadProgressNotSuppressedAcrossSteps(t *testing.T) {
	h := &recordingHandle{}
	p := &InstallProgress{}
	p.SetJobHandle(h)

	p.SetDownloadProgress(0, 1000, 0)
	before := len(h.progress)

	p.SetStep(2, 5, "downloading the second artifact")
	p.SetDownloadProgress(0, 2000, 0)

	assert.Greater(t, len(h.progress), before+1,
		"step change must let the next download's first frame through")
}
