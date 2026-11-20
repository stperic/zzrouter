package calibration

import (
	"context"
	"maps"
	"sync"
	"sync/atomic"
	"time"
)

// SamplingInterval spaces resource and engine observations.
const SamplingInterval = 500 * time.Millisecond

// Clock allows the caller to control timestamps and timers together.
type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

// Sender measures a pinned request and uses the supplied clock for lease and content timing.
type Sender interface {
	// Send returns promptly when ctx ends and releases its lease before returning.
	Send(ctx context.Context, clock Clock, runID, targetRevision string, body map[string]any) Sample
}

// Sample holds lease and token measurements from the supplied clock, including any lease granted before cancellation.
type Sample struct {
	LeaseGranted      time.Time
	SlotWait          *time.Duration
	FirstContentToken time.Time
	LastContentToken  time.Time
	InputTokens       *int
	OutputTokens      *int
	ErrorCode         string // Use the existing request error code and leave it empty for a ctx interruption.
}

// Sampler observes the checked runs and pools and returns immutable data.
type Sampler interface {
	// Sample returns promptly when ctx ends, at which point any partial snapshot is discarded.
	Sample(ctx context.Context) Snapshot
}

// Snapshot contains one node resource sample and corresponding run observations.
type Snapshot struct {
	Pools []PoolSample
	Runs  map[string]RunSample
}

// PoolSample contains the latest resource measurements, with nil for unavailable values.
type PoolSample struct {
	ID, Kind                                              string
	Vendor                                                *string
	TotalMiB, UsedMiB, FreeMiB, AvailableMiB, SwapUsedMiB *int64
	UtilizationPct, PowerW, CPUPct                        *float64
	Holders                                               []Holder
	HostMemoryMiB                                         map[string]*int64
}

// RunSample contains engine and counter observations. An empty revision signals a stopped run.
type RunSample struct {
	TargetRevision   string
	RequestsAdmitted *uint64
	Running, Waiting *int
	KVCacheUsage     *float64
	CPUPct           *float64
}

// Target supplies identity and sanitized report fields captured at submission.
type Target struct {
	TargetRevision string
	Run            RunReport
	Output         map[string]any // A non-nil declaration forces chat output length.
}

// Progress supplies counts and the latest snapshot on the runner's sampling cadence.
type Progress struct {
	Step, Steps                      int
	RequestsCompleted, RequestsTotal int
	Snapshot                         Snapshot
}

// Runner schedules calibration using the node's sender, sampler and clock.
type Runner struct {
	Sender   Sender
	Sampler  Sampler
	Clock    Clock
	Progress func(Progress)
}

type measuredRequest struct {
	Sample
	sentAt      time.Time
	completedAt time.Time
	interrupted bool
}

type preparedRun struct {
	Target
	workload     *Workload
	characters   int
	warmupReason *Reason
}

type stepRun struct {
	preparedRun
	concurrency int
	requests    []measuredRequest
}

type sampleSeries struct {
	previous time.Time
	gap      *float64
}

func (s *sampleSeries) sample(ctx context.Context, r Runner) (Snapshot, bool) {
	if ctx.Err() != nil {
		return Snapshot{}, false
	}
	snapshot := r.Sampler.Sample(ctx)
	now := r.Clock.Now()
	if ctx.Err() != nil {
		return Snapshot{}, false
	}
	if !s.previous.IsZero() {
		s.gap = peak(s.gap, new(ms(now.Sub(s.previous))))
	}
	s.previous = now
	return snapshot, true
}

// Run measures a validated request with a checked target for each run ID and returns any cancellation cause.
func (r Runner) Run(ctx context.Context, request Request, targets map[string]Target) (Report, error) {
	report := Report{
		StartedAt: r.Clock.Now().UTC(), Request: request, SamplingIntervalMS: SamplingInterval.Milliseconds(),
		Steps: []StepReport{}, Resources: []PoolReport{},
	}
	gen := newGenerator()
	series := sampleSeries{}
	previous := make(map[string]RunSample, len(targets))
	if snapshot, ok := series.sample(ctx, r); ok {
		maps.Copy(previous, snapshot.Runs)
		report.Resources = checkPools(snapshot.Pools, targets)
	}
	runs, steps := prepareRuns(request, targets, &report)
	limit := request.stepTimeout()
	r.warmup(ctx, limit, runs, gen)
	for i := 0; i < steps && ctx.Err() == nil; i++ {
		step := r.measureStep(ctx, i, steps, limit, runs, gen, &series, previous, targets)
		if step != nil {
			report.Steps = append(report.Steps, *step)
		}
	}
	cause := context.Cause(ctx)
	report.Outcome, report.Reason = reportOutcome(report.Steps, cause != nil)
	report.CompletedAt = r.Clock.Now().UTC()
	report.LargestSampleGapMS = series.gap
	return report, cause
}

func prepareRuns(request Request, targets map[string]Target, report *Report) ([]preparedRun, int) {
	runs := make([]preparedRun, len(request.Runs))
	report.Runs = make([]RunReport, len(runs))
	steps := 0
	for i, entry := range request.Runs {
		target := targets[entry.RunID]
		view := target.Run
		view.RunID = entry.RunID
		view.Overridden = append([]string{}, view.Overridden...)
		view.ObservedRoles = append([]string{}, view.ObservedRoles...)
		if view.ParametersStatus == "" {
			view.ParametersStatus = "unknown"
		}
		if entry.Workload != nil {
			steps = len(entry.Workload.Concurrency)
			view.WorkloadRevision = new(WorkloadRevision(view.Endpoint))
			view.OutputForced = view.Endpoint == Chat && target.Output != nil
		}
		target.Run = view
		report.Runs[i] = view
		runs[i] = preparedRun{Target: target, workload: entry.Workload}
	}
	return runs, steps
}

func (r Runner) warmup(ctx context.Context, limit time.Duration, runs []preparedRun, gen *generator) {
	ctx, stop := r.withTimeout(ctx, limit)
	defer stop()
	var workers sync.WaitGroup
	for i := range runs {
		run := &runs[i]
		if run.workload == nil {
			continue
		}
		workers.Go(func() {
			short := r.warmupRequest(ctx, gen, run.Target, shortWarmupCharacters)
			long := r.warmupRequest(ctx, gen, run.Target, longWarmupCharacters)
			if context.Cause(ctx) == context.DeadlineExceeded {
				run.warmupReason = reason(stepTimeout)
				return
			}
			run.characters, run.warmupReason = promptSize(run.workload.InputTokens, short, long)
		})
	}
	workers.Wait()
}

func (r Runner) warmupRequest(ctx context.Context, gen *generator, target Target, characters int) Sample {
	if ctx.Err() != nil {
		return Sample{}
	}
	sample := r.Sender.Send(ctx, r.Clock, target.Run.RunID, target.TargetRevision, gen.body(target, characters, 1))
	if ctx.Err() != nil && sample.ErrorCode == "" {
		return Sample{}
	}
	return sample
}

func (r Runner) withTimeout(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	alarm := r.Clock.After(limit)
	go func() {
		select {
		case <-alarm:
			cancel(context.DeadlineExceeded)
		case <-ctx.Done():
		}
	}()
	return ctx, func() { cancel(nil) }
}

type stepFinish struct {
	at    time.Time
	cause error
}

func (r Runner) measureStep(ctx context.Context, index, steps int, limit time.Duration, prepared []preparedRun, gen *generator, series *sampleSeries, previous map[string]RunSample, targets map[string]Target) *StepReport {
	stepCtx, stopStep := r.withTimeout(ctx, limit)
	defer stopStep()
	started := r.Clock.Now()
	nextSample := started.Add(SamplingInterval)
	series.previous = time.Time{}
	var snapshots []Snapshot
	if first, ok := series.sample(stepCtx, r); ok {
		snapshots = append(snapshots, first)
	}
	probeCtx, stopProbes := context.WithCancel(stepCtx)
	defer stopProbes()
	var completed atomic.Int64
	var workers sync.WaitGroup
	runs, total := r.startPools(stepCtx, &workers, prepared, gen, index, &completed)
	finished := make(chan stepFinish, 1)
	go func() {
		workers.Wait()
		finished <- stepFinish{at: r.Clock.Now(), cause: context.Cause(stepCtx)}
		stopProbes()
	}()

	for probeCtx.Err() == nil {
		tick := r.Clock.After(max(0, nextSample.Sub(r.Clock.Now())))
		select {
		case <-probeCtx.Done():
		case <-tick:
			if probeCtx.Err() != nil {
				break
			}
			nextSample = r.Clock.Now().Add(SamplingInterval)
			if snapshot, ok := series.sample(probeCtx, r); ok {
				snapshots = append(snapshots, snapshot)
				r.publish(index, steps, int(completed.Load()), total, snapshot)
			}
		}
	}
	end := <-finished
	if last, ok := series.sample(stepCtx, r); ok {
		snapshots = append(snapshots, last)
		r.publish(index, steps, int(completed.Load()), total, last)
	}
	if context.Cause(ctx) != nil {
		return nil
	}
	timedOut := end.cause == context.DeadlineExceeded
	window := end.at.Sub(started)
	if timedOut {
		window = limit
	}
	observed := stepObservations{snapshots: snapshots, through: series.previous}
	return new(summarizeStep(index, window, runs, observed, previous, timedOut, targets))
}

func (r Runner) publish(index, steps, completed, total int, snapshot Snapshot) {
	if r.Progress != nil {
		r.Progress(Progress{Step: index, Steps: steps, RequestsCompleted: completed, RequestsTotal: total, Snapshot: snapshot})
	}
}

func (r Runner) startPools(ctx context.Context, workers *sync.WaitGroup, prepared []preparedRun, gen *generator, index int, completed *atomic.Int64) ([]stepRun, int) {
	runs := make([]stepRun, 0, len(prepared))
	total := 0
	for _, run := range prepared {
		w := run.workload
		if w == nil || w.Concurrency[index] == 0 {
			continue
		}
		concurrency := w.Concurrency[index]
		count := w.requestCount(concurrency)
		requests := make([]measuredRequest, count)
		runs = append(runs, stepRun{preparedRun: run, concurrency: concurrency, requests: requests})
		total += count
		if run.warmupReason == nil {
			var next atomic.Int64
			for range concurrency {
				workers.Go(func() { r.sendPool(ctx, run, gen, requests, &next, completed) })
			}
		}
	}
	return runs, total
}

func (r Runner) sendPool(ctx context.Context, run preparedRun, gen *generator, requests []measuredRequest, next, completed *atomic.Int64) {
	output := 0
	if run.workload.OutputTokens != nil {
		output = *run.workload.OutputTokens
	}
	for ctx.Err() == nil {
		i := int(next.Add(1) - 1)
		if i >= len(requests) {
			return
		}
		body := gen.body(run.Target, run.characters, output)
		if ctx.Err() != nil {
			return
		}
		sent := r.Clock.Now()
		sample := r.Sender.Send(ctx, r.Clock, run.Run.RunID, run.TargetRevision, body)
		finished := r.Clock.Now()
		requests[i] = measuredRequest{
			Sample: sample, sentAt: sent, completedAt: finished,
			interrupted: sample.ErrorCode == "" && ctx.Err() != nil,
		}
		completed.Add(1)
	}
}
