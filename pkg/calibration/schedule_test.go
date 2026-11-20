package calibration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stperic/zzrouter/pkg/utils/clock/clocktest"
	"github.com/stretchr/testify/require"
)

var epoch = time.Date(2026, 11, 19, 15, 0, 0, 0, time.UTC)

type senderFunc func(context.Context, Clock, string, string, map[string]any) Sample

func (f senderFunc) Send(ctx context.Context, clock Clock, id, revision string, body map[string]any) Sample {
	sample := f(ctx, clock, id, revision, body)
	sample.TargetRevision = revision
	return sample
}

type samplerFunc func(context.Context) Snapshot

func (f samplerFunc) Sample(ctx context.Context) Snapshot         { return f(ctx) }
func (f samplerFunc) Revisions(context.Context) map[string]string { return nil }

func runnerFixture(endpoint string) (Runner, *clocktest.FakeClock, map[string]Target) {
	clock := clocktest.NewFakeClock(epoch)
	targets := map[string]Target{
		"a":        {TargetRevision: "a-original", Run: RunReport{Endpoint: endpoint, Model: "a"}},
		"b":        {TargetRevision: "b-original", Run: RunReport{Endpoint: endpoint, Model: "b"}},
		"resident": {TargetRevision: "r-original", Run: RunReport{Endpoint: "images", Model: "resident"}},
	}
	return Runner{Clock: clock, Sampler: samplerFunc(func(context.Context) Snapshot { return Snapshot{} })}, clock, targets
}

func isWarmup(body map[string]any) bool { return body["max_tokens"] == 1 }

func warmupSample(body map[string]any) Sample {
	messages, _ := body["messages"].([]map[string]string)
	return Sample{InputTokens: new(len(messages[0]["content"]) / 16)}
}

func completedSample(when time.Time) Sample {
	return Sample{
		LeaseGranted: when, SlotWait: new(time.Duration(0)), FirstContentToken: when,
		LastContentToken: when, InputTokens: new(10), OutputTokens: new(2),
	}
}

type runResult struct {
	report Report
	err    error
}

func startTestRun(ctx context.Context, runner Runner, request Request, targets map[string]Target) <-chan runResult {
	done := make(chan runResult, 1)
	go func() {
		report, err := runner.Run(ctx, request, targets)
		done <- runResult{report, err}
	}()
	return done
}

func TestRunnerRecoversRequestWorkerPanics(t *testing.T) {
	for _, phase := range []string{WarmingUp, Measuring} {
		t.Run(phase, func(t *testing.T) {
			const panicValue = "private worker panic fixture"
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			runner, _, targets := runnerFixture(Chat)
			runner.Sender = senderFunc(func(_ context.Context, now Clock, _, _ string, body map[string]any) Sample {
				if isWarmup(body) == (phase == WarmingUp) {
					panic(panicValue)
				}
				if isWarmup(body) {
					return warmupSample(body)
				}
				return completedSample(now.Now())
			})
			report, err := runner.Run(t.Context(), testRequest(chatWorkload(1)), targets)
			require.ErrorIs(t, err, ErrRunnerPanic)
			payload, err := json.Marshal(report)
			require.NoError(t, err)
			require.NotContains(t, string(payload), panicValue)
			require.Contains(t, logs.String(), `"level":"ERROR"`)
			require.Contains(t, logs.String(), `"panic":"`+panicValue+`"`)
			require.Contains(t, logs.String(), `"stack":"goroutine `)
		})
	}
}

func TestRunnerInFlightPools(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner, clock, targets := runnerFixture(Chat)
		workB := chatWorkload(4)
		workB.RequestsPerStep = new(7)
		request := testRequest(chatWorkload(2), workB, nil)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		started := make(chan string, 32)
		permits := map[string]chan struct{}{"a": make(chan struct{}, 2), "b": make(chan struct{}, 4)}
		runner.Sender = senderFunc(func(ctx context.Context, now Clock, id, rev string, body map[string]any) Sample {
			if rev != id+"-original" {
				t.Errorf("unexpected binding %s for run %s", rev, id)
			}
			if isWarmup(body) {
				return warmupSample(body)
			}
			lease := now.Now()
			started <- id
			select {
			case <-ctx.Done():
				return Sample{LeaseGranted: lease}
			case <-permits[id]:
				sample := completedSample(now.Now())
				sample.LeaseGranted = lease
				return sample
			}
		})
		var finalProgress Progress
		runner.Progress = func(p Progress) { finalProgress = p }
		done := startTestRun(ctx, runner, request, targets)
		totals := map[string]int{}
		for _, want := range []map[string]int{
			{"a": 2, "b": 4}, {"a": 2, "b": 3}, {"a": 2}, {"a": 2}, {"a": 2},
		} {
			synctest.Wait()
			select {
			case result := <-done:
				t.Fatalf("finished before exhausting request counts: %+v", result)
			default:
			}
			batch := map[string]int{}
			for len(started) > 0 {
				batch[<-started]++
			}
			require.Equal(t, want, batch, "all runs should fill their own pool up to the requested concurrency")
			clock.Advance(10 * time.Millisecond)
			for id, count := range batch {
				totals[id] += count
				for range count {
					permits[id] <- struct{}{}
				}
			}
		}
		synctest.Wait()
		require.Len(t, done, 1, "a step should end at its request count")
		result := <-done
		require.NoError(t, result.err)
		require.Equal(t, map[string]int{"a": 10, "b": 7}, totals)
		require.Len(t, started, 0)
		require.Len(t, result.report.Runs, 3)
		require.Nil(t, result.report.Runs[2].WorkloadRevision)
		require.Len(t, result.report.Steps, 1)
		step := result.report.Steps[0]
		require.Equal(t, 50.0, step.DurationMS)
		require.Len(t, step.Runs, 2)
		require.Equal(t, RequestCounts{Requested: 10, Completed: 10}, step.Runs[0].Requests)
		require.Equal(t, RequestCounts{Requested: 7, Completed: 7}, step.Runs[1].Requests)
		require.Equal(t, 680.0, *step.Throughput.OutputTokensPerSecond)
		require.Equal(t, map[string]RequestCounts{
			"a": {Requested: 10, Completed: 10},
			"b": {Requested: 7, Completed: 7},
		}, finalProgress.Runs)
	})
}

func TestRunnerStepTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner, clock, targets := runnerFixture(Chat)
		request := testRequest(chatWorkload(2))
		request.StepTimeoutSeconds = new(1)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		causes := make(chan error, 10)
		var sends atomic.Int64
		runner.Sender = senderFunc(func(ctx context.Context, _ Clock, _, _ string, body map[string]any) Sample {
			if isWarmup(body) {
				return warmupSample(body)
			}
			sends.Add(1)
			<-ctx.Done()
			causes <- context.Cause(ctx)
			return Sample{}
		})
		done := startTestRun(ctx, runner, request, targets)
		synctest.Wait()
		require.Equal(t, int64(2), sends.Load())
		clock.Advance(999 * time.Millisecond)
		synctest.Wait()
		require.Empty(t, causes)
		require.Empty(t, done)
		clock.Advance(time.Millisecond)
		synctest.Wait()
		require.Len(t, causes, 2)
		require.ErrorIs(t, <-causes, context.DeadlineExceeded)
		require.ErrorIs(t, <-causes, context.DeadlineExceeded)
		require.Len(t, done, 1)
		result := <-done
		require.NoError(t, result.err)
		requireVerdict(t, "inconclusive", "step_timeout", result.report.Outcome, result.report.Reason)
		require.Len(t, result.report.Steps, 1)
		step := result.report.Steps[0]
		require.Equal(t, 1000.0, step.DurationMS)
		requireVerdict(t, "inconclusive", "step_timeout", step.Outcome, step.Reason)
		requireVerdict(t, "inconclusive", "step_timeout", step.Runs[0].Outcome, step.Runs[0].Reason)
		require.Equal(t, RequestCounts{Requested: 10}, step.Runs[0].Requests)
		require.Equal(t, int64(2), sends.Load())
	})
}

func TestRunnerCancellationKeepsMeasuredSteps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner, _, targets := runnerFixture(Chat)
		workA, workB := chatWorkload(1, 0, 1), chatWorkload(0, 1, 0)
		workA.RequestsPerStep, workB.RequestsPerStep = new(1), new(1)
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		cause := errors.New("job cancelled by caller")
		observed := make(chan error, 1)
		runner.Sender = senderFunc(func(ctx context.Context, now Clock, id, _ string, body map[string]any) Sample {
			if isWarmup(body) {
				return warmupSample(body)
			}
			if id == "a" {
				return completedSample(now.Now())
			}
			<-ctx.Done()
			observed <- context.Cause(ctx)
			return Sample{}
		})
		done := startTestRun(ctx, runner, testRequest(workA, workB), targets)
		synctest.Wait()
		require.Empty(t, done)
		cancel(cause)
		synctest.Wait()
		require.Len(t, observed, 1)
		require.ErrorIs(t, <-observed, cause)
		require.Len(t, done, 1)
		result := <-done
		require.ErrorIs(t, result.err, cause)
		requireVerdict(t, "cancelled", "cancelled", result.report.Outcome, result.report.Reason)
		require.Len(t, result.report.Steps, 1)
		step := result.report.Steps[0]
		require.Equal(t, verified, step.Outcome)
		require.Len(t, step.Runs, 1)
		require.Equal(t, "a", step.Runs[0].RunID)
	})
}

func TestRunnerSamplingCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner, clock, targets := runnerFixture(Chat)
		w := chatWorkload(1)
		w.RequestsPerStep = new(1)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		samples := 0
		unblockSample := make(chan struct{})
		unblockSender := make(chan struct{})
		runner.Sampler = samplerFunc(func(ctx context.Context) Snapshot {
			samples++
			if samples == 3 {
				select {
				case <-unblockSample:
				case <-ctx.Done():
				}
			}
			return Snapshot{}
		})
		runner.Sender = senderFunc(func(ctx context.Context, now Clock, _, _ string, body map[string]any) Sample {
			if isWarmup(body) {
				return warmupSample(body)
			}
			select {
			case <-unblockSender:
				return completedSample(now.Now())
			case <-ctx.Done():
				return Sample{}
			}
		})
		var progress []Progress
		runner.Progress = func(update Progress) { progress = append(progress, update) }
		done := startTestRun(ctx, runner, testRequest(w), targets)
		synctest.Wait()
		require.Equal(t, 2, samples)
		clock.Advance(500 * time.Millisecond)
		synctest.Wait()
		require.Equal(t, 3, samples)
		clock.Advance(700 * time.Millisecond)
		synctest.Wait()
		require.Equal(t, 3, samples, "a slow snapshot must not overlap with the next")
		close(unblockSample)
		synctest.Wait()
		clock.Advance(0)
		synctest.Wait()
		require.Equal(t, 4, samples)
		close(unblockSender)
		synctest.Wait()
		require.Len(t, done, 1)
		result := <-done
		require.NoError(t, result.err)
		require.Equal(t, 5, samples, "the final sample closes the public request counter window")
		require.Equal(t, int64(500), result.report.SamplingIntervalMS)
		require.Equal(t, 1200.0, *result.report.LargestSampleGapMS)
		require.Len(t, progress, 6)
		require.Equal(t, Progress{Phase: Checking, Steps: 1, SampleAt: epoch}, progress[0])
		require.Equal(t, Progress{Phase: WarmingUp, Steps: 1, SampleAt: epoch}, progress[1])
		require.Equal(t, Progress{Phase: Measuring, Steps: 1, SampleAt: epoch, RequestsTotal: 1, Runs: map[string]RequestCounts{"a": {Requested: 1}}}, progress[2])
		require.Equal(t, epoch.Add(1200*time.Millisecond), progress[3].SampleAt)
		require.Equal(t, Progress{Phase: Measuring, Steps: 1, SampleAt: epoch.Add(1200 * time.Millisecond), RequestsTotal: 1, RequestsCompleted: 1, Runs: map[string]RequestCounts{"a": {Requested: 1, Completed: 1}}}, progress[5])
	})
}

func TestRunnerAdmissionObservations(t *testing.T) {
	for _, tt := range []struct {
		name      string
		mid, end  RunSample
		code      string
		outcome   Outcome
		public    *uint64
		errorCode string
	}{
		{"public requests", runObservation("a-original", 14), runObservation("a-original", 16), "", "verified", new(uint64(5)), ""},
		{"revision changed", runObservation("different", 14), runObservation("a-original", 16), "run_changed", "inconclusive", nil, ""},
		{"revision changed and request failed", runObservation("replacement", 14), runObservation("replacement", 16), "run_changed", "inconclusive", nil, "upstream_stream_error"},
		{"run stopped", runObservation("", 14), runObservation("a-original", 16), "run_changed", "inconclusive", nil, ""},
		{"counter went backwards", runObservation("a-original", 9), runObservation("a-original", 16), "run_changed", "inconclusive", nil, ""},
		{"unknown counter", runObservation("a-original", 14), RunSample{TargetRevision: "a-original"}, "", "verified", nil, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runner, clock, targets := runnerFixture(Chat)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				w := chatWorkload(1)
				w.RequestsPerStep = new(1)
				observations := []RunSample{runObservation("a-original", 9), runObservation("a-original", 10), tt.mid, tt.end}
				i := 0
				runner.Sampler = samplerFunc(func(context.Context) Snapshot {
					s := Snapshot{Runs: map[string]RunSample{"a": observations[i]}}
					i++
					return s
				})
				release := make(chan struct{})
				runner.Sender = senderFunc(func(ctx context.Context, now Clock, _, _ string, body map[string]any) Sample {
					if isWarmup(body) {
						return warmupSample(body)
					}
					select {
					case <-release:
						if tt.errorCode != "" {
							return Sample{ErrorCode: tt.errorCode}
						}
						return completedSample(now.Now())
					case <-ctx.Done():
						return Sample{}
					}
				})
				done := startTestRun(ctx, runner, testRequest(w), targets)
				synctest.Wait()
				clock.Advance(SamplingInterval)
				synctest.Wait()
				require.Equal(t, 3, i)
				close(release)
				synctest.Wait()
				require.Len(t, done, 1)
				result := <-done
				require.NoError(t, result.err)
				require.Len(t, result.report.Steps, 1)
				run := result.report.Steps[0].Runs[0]
				requireVerdict(t, tt.outcome, tt.code, run.Outcome, run.Reason)
				require.Equal(t, tt.public, run.PublicRequests)
				if tt.errorCode != "" {
					require.Equal(t, RequestCounts{Requested: 1, Failed: 1}, run.Requests)
					requireVerdict(t, tt.outcome, tt.code, result.report.Outcome, result.report.Reason)
				}
			})
		})
	}
}

func runObservation(revision string, count uint64) RunSample {
	return RunSample{TargetRevision: revision, RequestsAdmitted: &count}
}

func TestRunnerTimeoutIncludesBoundarySamples(t *testing.T) {
	for _, tt := range []struct {
		name      string
		sample    int
		completed int
		outcome   Outcome
		reason    string
		duration  float64
	}{
		{"opening snapshot", 2, 0, "inconclusive", "step_timeout", 1000},
		{"closing snapshot", 3, 1, "verified", "", 125},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runner, clock, targets := runnerFixture(Chat)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				samples := 0
				runner.Sampler = samplerFunc(func(ctx context.Context) Snapshot {
					samples++
					if samples == tt.sample {
						<-ctx.Done()
					}
					return Snapshot{}
				})
				runner.Sender = senderFunc(func(_ context.Context, now Clock, _, _ string, body map[string]any) Sample {
					if isWarmup(body) {
						return warmupSample(body)
					}
					clock.Advance(125 * time.Millisecond)
					return completedSample(now.Now())
				})
				w := chatWorkload(1)
				w.RequestsPerStep = new(1)
				request := testRequest(w)
				request.StepTimeoutSeconds = new(1)
				done := startTestRun(ctx, runner, request, targets)
				synctest.Wait()
				require.Equal(t, tt.sample, samples)
				require.Empty(t, done)
				clock.Advance(time.Second)
				synctest.Wait()
				require.Len(t, done, 1)
				result := <-done
				require.NoError(t, result.err)
				requireVerdict(t, tt.outcome, tt.reason, result.report.Outcome, result.report.Reason)
				require.Nil(t, result.report.LargestSampleGapMS)
				step := result.report.Steps[0]
				require.Equal(t, tt.duration, step.DurationMS)
				require.Equal(t, tt.completed, step.Runs[0].Requests.Completed)
			})
		})
	}
}

func TestRunnerDiscardsCancelledProbeAndKeepsPublicCount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner, clock, targets := runnerFixture(Chat)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		w := chatWorkload(2)
		w.RequestsPerStep = new(2)
		request := testRequest(w)
		request.StepTimeoutSeconds = new(2)
		samples := 0
		partialReturned := make(chan struct{})
		runner.Sampler = samplerFunc(func(ctx context.Context) Snapshot {
			samples++
			observation := runObservation("a-original", 10)
			observation.Running = new(1)
			switch samples {
			case 3:
				observation.RequestsAdmitted = new(uint64(16))
			case 4:
				<-ctx.Done()
				observation = runObservation("replacement", 99)
				observation.Running = new(99)
				close(partialReturned)
			}
			return Snapshot{Runs: map[string]RunSample{"a": observation}}
		})
		var sent atomic.Int64
		grantSecond := make(chan struct{})
		runner.Sender = senderFunc(func(ctx context.Context, now Clock, _, _ string, body map[string]any) Sample {
			if isWarmup(body) {
				return warmupSample(body)
			}
			if sent.Add(1) == 2 {
				select {
				case <-grantSecond:
				case <-ctx.Done():
					return Sample{}
				}
			}
			granted := now.Now()
			<-ctx.Done()
			return Sample{LeaseGranted: granted}
		})
		var progress []Progress
		runner.Progress = func(update Progress) { progress = append(progress, update) }
		done := startTestRun(ctx, runner, request, targets)
		synctest.Wait()
		require.Equal(t, int64(2), sent.Load())
		clock.Advance(500 * time.Millisecond)
		synctest.Wait()
		require.Equal(t, 3, samples)
		clock.Advance(250 * time.Millisecond)
		close(grantSecond)
		synctest.Wait()
		clock.Advance(250 * time.Millisecond)
		synctest.Wait()
		require.Equal(t, 4, samples)
		clock.Advance(time.Second)
		synctest.Wait()
		require.Len(t, done, 1)
		requireClosed(t, partialReturned)
		result := <-done
		require.NoError(t, result.err)
		run := result.report.Steps[0].Runs[0]
		requireVerdict(t, "inconclusive", "step_timeout", run.Outcome, run.Reason)
		require.Equal(t, new(uint64(5)), run.PublicRequests)
		require.Equal(t, new(1), run.Engine.Running)
		require.Equal(t, new(500.0), result.report.LargestSampleGapMS)
		require.Len(t, progress, 4)
	})
}

func requireClosed(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	default:
		t.Fatal("the probe should have returned partial data")
	}
}

func TestRunnerGapExcludesWarmupAndStepBoundaries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner, clock, targets := runnerFixture(Chat)
		w := chatWorkload(1, 1)
		w.RequestsPerStep = new(1)
		observations, measuredRequests := 0, 0
		runner.Sampler = samplerFunc(func(context.Context) Snapshot {
			observations++
			if observations == 4 {
				clock.Advance(4 * time.Second)
			}
			return Snapshot{}
		})
		runner.Sender = senderFunc(func(_ context.Context, now Clock, _, _ string, body map[string]any) Sample {
			if isWarmup(body) {
				clock.Advance(3 * time.Second)
				return warmupSample(body)
			}
			measuredRequests++
			if measuredRequests == 1 {
				clock.Advance(200 * time.Millisecond)
			}
			return completedSample(now.Now())
		})
		report, err := runner.Run(t.Context(), testRequest(w), targets)
		require.NoError(t, err)
		require.Equal(t, verified, report.Outcome)
		require.Len(t, report.Steps, 2)
		require.Equal(t, 5, observations)
		require.Equal(t, 10_200*time.Millisecond, report.CompletedAt.Sub(report.StartedAt))
		require.Equal(t, new(200.0), report.LargestSampleGapMS)
	})
}

func TestRunnerWarmupSharesOneDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner, clock, targets := runnerFixture(Chat)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		request := testRequest(chatWorkload(1, 1))
		request.StepTimeoutSeconds = new(1)
		sends := 0
		cause := make(chan error, 1)
		runner.Sender = senderFunc(func(ctx context.Context, _ Clock, _, _ string, body map[string]any) Sample {
			sends++
			switch sends {
			case 1:
				clock.Advance(300 * time.Millisecond)
				return warmupSample(body)
			case 2:
				<-ctx.Done()
				cause <- context.Cause(ctx)
				return Sample{}
			default:
				t.Error("the measured request requires two usable warm-up replies")
				return Sample{}
			}
		})
		done := startTestRun(ctx, runner, request, targets)
		synctest.Wait()
		require.Equal(t, 2, sends)
		clock.Advance(699 * time.Millisecond)
		synctest.Wait()
		require.Empty(t, cause)
		require.Empty(t, done)
		clock.Advance(time.Millisecond)
		synctest.Wait()
		require.Len(t, cause, 1)
		require.ErrorIs(t, <-cause, context.DeadlineExceeded)
		require.Len(t, done, 1)
		result := <-done
		require.NoError(t, result.err)
		requireVerdict(t, "inconclusive", "step_timeout", result.report.Outcome, result.report.Reason)
		require.Len(t, result.report.Steps, 2)
		for _, step := range result.report.Steps {
			requireVerdict(t, "inconclusive", "step_timeout", step.Outcome, step.Reason)
			require.Len(t, step.Runs, 1)
			run := step.Runs[0]
			requireVerdict(t, "inconclusive", "step_timeout", run.Outcome, run.Reason)
			require.Equal(t, RequestCounts{Requested: 10}, run.Requests)
		}
		require.Equal(t, 2, sends)
	})
}

func TestRunnerStampsRequestWindow(t *testing.T) {
	runner, clock, targets := runnerFixture(Chat)
	w := chatWorkload(1)
	w.RequestsPerStep = new(1)
	runner.Sender = senderFunc(func(_ context.Context, now Clock, _, _ string, body map[string]any) Sample {
		require.Same(t, clock, now)
		if isWarmup(body) {
			return warmupSample(body)
		}
		clock.Advance(20 * time.Millisecond)
		sample := completedSample(now.Now())
		sample.SlotWait = new(20 * time.Millisecond)
		clock.Advance(30 * time.Millisecond)
		sample.FirstContentToken = now.Now()
		clock.Advance(50 * time.Millisecond)
		sample.LastContentToken = now.Now()
		clock.Advance(60 * time.Millisecond)
		return sample
	})
	report, err := runner.Run(t.Context(), testRequest(w), targets)
	require.NoError(t, err)
	require.Equal(t, verified, report.Outcome)
	run := report.Steps[0].Runs[0]
	require.Equal(t, 20.0, run.SlotWaitMS.P50)
	require.Equal(t, 30.0, run.TimeToFirstTokenMS.P50)
	require.NotNil(t, run.InterTokenLatencyMS)
	require.Equal(t, 110.0, run.InterTokenLatencyMS.P50)
	require.Equal(t, new(6.25), run.Throughput.RequestsPerSecond)
}

func TestRunnerTimeoutAppliesOnlyToUnfinishedRun(t *testing.T) {
	for _, tt := range []struct {
		name       string
		earlyError string
		want       Outcome
		code       string
		counts     RequestCounts
	}{
		{"slow run times out", "", "inconclusive", "step_timeout", RequestCounts{Requested: 2}},
		{"failed request outranks timeout", "upstream_timeout", "failed", "requests_failed", RequestCounts{Requested: 2, Failed: 1}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runner, clock, targets := runnerFixture(Chat)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				a, b := chatWorkload(1), chatWorkload(1)
				a.RequestsPerStep, b.RequestsPerStep = new(1), new(2)
				request := testRequest(a, b)
				request.StepTimeoutSeconds = new(1)
				slowRequests := 0
				runner.Sender = senderFunc(func(ctx context.Context, now Clock, id, _ string, body map[string]any) Sample {
					if isWarmup(body) {
						return warmupSample(body)
					}
					if id == "a" {
						return completedSample(now.Now())
					}
					slowRequests++
					if slowRequests == 1 && tt.earlyError != "" {
						return Sample{ErrorCode: tt.earlyError}
					}
					<-ctx.Done()
					return Sample{}
				})
				done := startTestRun(ctx, runner, request, targets)
				synctest.Wait()
				require.Empty(t, done)
				clock.Advance(time.Second)
				synctest.Wait()
				require.Len(t, done, 1)
				result := <-done
				require.NoError(t, result.err)
				step := result.report.Steps[0]
				requireVerdict(t, "verified", "", step.Runs[0].Outcome, step.Runs[0].Reason)
				require.Equal(t, RequestCounts{Requested: 1, Completed: 1}, step.Runs[0].Requests)
				requireVerdict(t, tt.want, tt.code, step.Runs[1].Outcome, step.Runs[1].Reason)
				require.Equal(t, tt.counts, step.Runs[1].Requests)
				requireVerdict(t, tt.want, tt.code, step.Outcome, step.Reason)
				requireVerdict(t, tt.want, tt.code, result.report.Outcome, result.report.Reason)
			})
		})
	}
}

func TestRunnerCancelsProbeAtRequestCount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner, clock, targets := runnerFixture(Chat)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		w := chatWorkload(1)
		w.RequestsPerStep = new(1)
		samples := 0
		cancelCause := make(chan error, 1)
		runner.Sampler = samplerFunc(func(ctx context.Context) Snapshot {
			samples++
			observation := runObservation("a-original", 10)
			if samples == 3 {
				<-ctx.Done()
				cancelCause <- context.Cause(ctx)
				observation = runObservation("replacement", 1)
			}
			return Snapshot{Runs: map[string]RunSample{"a": observation}}
		})
		release := make(chan struct{})
		runner.Sender = senderFunc(func(ctx context.Context, now Clock, _, _ string, body map[string]any) Sample {
			if isWarmup(body) {
				return warmupSample(body)
			}
			select {
			case <-release:
				return completedSample(now.Now())
			case <-ctx.Done():
				return Sample{}
			}
		})
		done := startTestRun(ctx, runner, testRequest(w), targets)
		synctest.Wait()
		clock.Advance(SamplingInterval)
		synctest.Wait()
		require.Equal(t, 3, samples)
		close(release)
		synctest.Wait()
		require.Len(t, done, 1)
		require.Len(t, cancelCause, 1)
		require.ErrorIs(t, <-cancelCause, context.Canceled)
		require.Equal(t, 4, samples)
		result := <-done
		require.NoError(t, result.err)
		requireVerdict(t, "verified", "", result.report.Outcome, result.report.Reason)
	})
}
