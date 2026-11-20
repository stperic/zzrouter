package calibration

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

type rawSender senderFunc

func (f rawSender) Send(ctx context.Context, clock Clock, id, revision string, body map[string]any) Sample {
	return f(ctx, clock, id, revision, body)
}

type identitySampler struct {
	samplerFunc
	current func(context.Context) map[string]string
}

func (s identitySampler) Revisions(ctx context.Context) map[string]string { return s.current(ctx) }

func TestRunnerSenderRevisionSurvivesDiscardedProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner, clock, targets := runnerFixture(Chat)
		a, b := chatWorkload(1), chatWorkload(1)
		a.RequestsPerStep, b.RequestsPerStep = new(1), new(1)
		request := testRequest(a, b)
		request.StepTimeoutSeconds = new(1)
		require.NoError(t, request.Validate())

		restart := make(chan struct{})
		requests := make(chan string, 2)
		runner.Sender = rawSender(func(ctx context.Context, _ Clock, id, revision string, body map[string]any) Sample {
			if isWarmup(body) {
				return warmupSample(body)
			}
			requests <- id
			if id == "a" {
				<-restart
				return Sample{ErrorCode: "run_changed"}
			}
			<-ctx.Done()
			return Sample{TargetRevision: revision}
		})
		var probes int
		runner.Sampler = samplerFunc(func(ctx context.Context) Snapshot {
			probes++
			revision := "a-original"
			if probes > 2 {
				<-ctx.Done()
				revision = "a-replacement"
			}
			return Snapshot{Runs: map[string]RunSample{"a": runObservation(revision, 10)}}
		})

		done := startTestRun(t.Context(), runner, request, targets)
		synctest.Wait()
		require.Len(t, requests, 2)
		clock.Advance(500 * time.Millisecond)
		synctest.Wait()
		require.Equal(t, 3, probes)
		clock.Advance(250 * time.Millisecond)
		close(restart)
		synctest.Wait()
		require.Empty(t, done)
		clock.Advance(250 * time.Millisecond)
		synctest.Wait()

		require.Len(t, done, 1)
		result := <-done
		require.NoError(t, result.err)
		run := result.report.Steps[0].Runs[0]
		require.NotNil(t, run.Reason)
		require.Equal(t, "run_changed", run.Reason.Code)
	})
}

func TestRunnerProgressIncludesOpeningSamples(t *testing.T) {
	runner, _, targets := runnerFixture(Chat)
	workload := chatWorkload(1)
	workload.RequestsPerStep = new(1)
	var sampled int
	var progress []Progress
	runner.Sender = senderFunc(func(_ context.Context, now Clock, _, _ string, body map[string]any) Sample {
		if isWarmup(body) {
			return warmupSample(body)
		}
		return completedSample(now.Now())
	})
	runner.Sampler = samplerFunc(func(context.Context) Snapshot {
		sampled++
		return Snapshot{Runs: map[string]RunSample{"a": runObservation("a-original", uint64(sampled))}}
	})
	runner.Progress = func(p Progress) { progress = append(progress, p) }

	report, err := runner.Run(t.Context(), testRequest(workload), targets)
	require.NoError(t, err)
	require.Equal(t, verified, report.Outcome)
	require.Equal(t, 3, sampled)
	require.Len(t, progress, 4)
	for i, phase := range []string{Checking, WarmingUp, Measuring, Measuring} {
		require.Equal(t, phase, progress[i].Phase)
		require.Equal(t, 1, progress[i].Steps)
	}
	for i, want := range []uint64{1, 1, 2, 3} {
		require.Equal(t, want, *progress[i].Snapshot.Runs["a"].RequestsAdmitted)
	}
	require.Equal(t, 1, progress[2].RequestsTotal)
	require.Zero(t, progress[2].RequestsCompleted)
	require.Equal(t, 1, progress[3].RequestsCompleted)
}

func TestRunnerEndRevisionAfterRequestsCompleted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner, clock, targets := runnerFixture(Chat)
		a, b := chatWorkload(1), chatWorkload(1)
		a.RequestsPerStep, b.RequestsPerStep = new(1), new(1)
		request := testRequest(a, b)
		request.StepTimeoutSeconds = new(1)
		revision := "a-original"
		sampled := ""
		runner.Sampler = identitySampler{
			samplerFunc: func(ctx context.Context) Snapshot {
				if clock.Now().Sub(epoch) >= SamplingInterval {
					<-ctx.Done()
				}
				sampled = revision
				return Snapshot{Runs: map[string]RunSample{"a": runObservation(revision, 10)}}
			},
			current: func(ctx context.Context) map[string]string {
				require.NoError(t, ctx.Err())
				_, bounded := ctx.Deadline()
				require.True(t, bounded)
				return map[string]string{"a": revision, "b": "b-original"}
			},
		}
		runner.Sender = senderFunc(func(ctx context.Context, clock Clock, id, _ string, body map[string]any) Sample {
			if isWarmup(body) {
				return warmupSample(body)
			}
			if id == "a" {
				return completedSample(clock.Now())
			}
			<-ctx.Done()
			return Sample{}
		})
		done := startTestRun(t.Context(), runner, request, targets)
		synctest.Wait()
		clock.Advance(500 * time.Millisecond)
		synctest.Wait()
		require.Equal(t, "a-original", sampled)
		revision = "a-replacement"
		clock.Advance(500 * time.Millisecond)
		synctest.Wait()
		report := (<-done).report
		require.Len(t, report.Steps, 1)
		gotA, gotB := report.Steps[0].Runs[0], report.Steps[0].Runs[1]
		require.Equal(t, 1, gotA.Requests.Completed)
		requireVerdict(t, inconclusive, "run_changed", gotA.Outcome, gotA.Reason)
		requireVerdict(t, inconclusive, "step_timeout", gotB.Outcome, gotB.Reason)
	})
}
