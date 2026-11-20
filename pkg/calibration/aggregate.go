package calibration

import (
	"cmp"
	"slices"
	"time"
)

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func percentiles(values []float64) *Percentiles {
	n := len(values)
	if n == 0 {
		return nil
	}
	slices.Sort(values)
	rank := func(p int) float64 { return values[(n*p+99)/100-1] }
	return &Percentiles{P50: rank(50), P90: rank(90), P99: rank(99), Min: values[0], Max: values[n-1], Count: n}
}

func rate(total float64, window time.Duration) *float64 {
	if window <= 0 {
		return nil
	}
	return new(total / window.Seconds())
}

func peak[T cmp.Ordered](a, b *T) *T {
	if a == nil || b != nil && *b > *a {
		return b
	}
	return a
}

func minimum(a, b *int64) *int64 {
	if a == nil || b != nil && *b < *a {
		return b
	}
	return a
}

type observations struct {
	peak  *float64
	sum   float64
	count int
}

func (o *observations) add(value *float64) {
	if value != nil {
		o.peak = peak(o.peak, value)
		o.sum += *value
		o.count++
	}
}

func (o observations) summary() *MeanPeak {
	if o.count == 0 {
		return nil
	}
	return &MeanPeak{Mean: o.sum / float64(o.count), Peak: *o.peak}
}

type tokenStats struct {
	bounds *TokenRange
	total  float64
	count  int
}

func (s *tokenStats) add(value *int) {
	if value == nil {
		return
	}
	if s.bounds == nil {
		s.bounds = &TokenRange{Min: *value, Max: *value}
	}
	s.bounds.Min = min(s.bounds.Min, *value)
	s.bounds.Max = max(s.bounds.Max, *value)
	s.total += float64(*value)
	s.count++
}

type streamEvent struct {
	at    time.Time
	delta int
}

func streamPeak(events []streamEvent) int {
	slices.SortFunc(events, func(a, b streamEvent) int {
		if order := a.at.Compare(b.at); order != 0 {
			return order
		}
		// A single-token stream emits at one instant and participates in the peak.
		return cmp.Compare(b.delta, a.delta)
	})
	current, peak := 0, 0
	for _, event := range events {
		current += event.delta
		peak = max(peak, current)
	}
	return peak
}

type requestStats struct {
	counts                    RequestCounts
	input, output             tokenStats
	waits, ttft, itl          []float64
	streams                   []streamEvent
	firstSend, lastCompletion time.Time
	admittedThrough           time.Time
	leases                    uint64
	firstError                string
}

func (s *requestStats) observe(sample measuredRequest, chat bool) {
	if sample.sentAt.IsZero() {
		return
	}
	if !sample.LeaseGranted.IsZero() && !sample.LeaseGranted.After(s.admittedThrough) {
		s.leases++
	}
	if chat && !sample.FirstContentToken.IsZero() {
		last := sample.LastContentToken
		if last.Before(sample.FirstContentToken) {
			last = sample.FirstContentToken
		}
		s.streams = append(s.streams, streamEvent{sample.FirstContentToken, 1}, streamEvent{last, -1})
	}
	if s.firstSend.IsZero() || sample.sentAt.Before(s.firstSend) {
		s.firstSend = sample.sentAt
	}
	if sample.interrupted {
		return
	}
	if sample.completedAt.After(s.lastCompletion) {
		s.lastCompletion = sample.completedAt
	}
	if sample.ErrorCode != "" {
		s.counts.Failed++
		if s.firstError == "" {
			s.firstError = sample.ErrorCode
		}
		return
	}
	s.counts.Completed++
	s.input.add(sample.InputTokens)
	if sample.SlotWait != nil {
		s.waits = append(s.waits, ms(*sample.SlotWait))
	}
	if chat {
		s.output.add(sample.OutputTokens)
		s.observeStreamTiming(sample)
	}
}

func (s *requestStats) observeStreamTiming(sample measuredRequest) {
	if sample.LeaseGranted.IsZero() || sample.FirstContentToken.Before(sample.LeaseGranted) {
		return
	}
	s.ttft = append(s.ttft, ms(sample.FirstContentToken.Sub(sample.LeaseGranted)))
	if sample.OutputTokens != nil && *sample.OutputTokens > 1 && !sample.completedAt.Before(sample.FirstContentToken) {
		s.itl = append(s.itl, ms(sample.completedAt.Sub(sample.FirstContentToken))/float64(*sample.OutputTokens-1))
	}
}

func (s requestStats) throughput() RunRates {
	window := s.lastCompletion.Sub(s.firstSend)
	rates := RunRates{RequestsPerSecond: rate(float64(s.counts.Completed), window)}
	if s.input.count > 0 && s.input.count == s.counts.Completed {
		rates.InputTokensPerSecond = rate(s.input.total, window)
	}
	if s.output.count > 0 && s.output.count == s.counts.Completed {
		rates.OutputTokensPerSecond = rate(s.output.total, window)
	}
	return rates
}

type stepObservations struct {
	snapshots []Snapshot
	through   time.Time
	revisions map[string]string
}

func summarizeRun(run stepRun, observed stepObservations, changed, stepTimedOut bool) (RunStep, *float64) {
	chat := run.Run.Endpoint == Chat
	stats := requestStats{
		counts:          RequestCounts{Requested: len(run.requests)},
		admittedThrough: observed.through,
	}
	for _, sample := range run.requests {
		changed = changed || (!sample.sentAt.IsZero() && sample.TargetRevision != run.TargetRevision)
		stats.observe(sample, chat)
	}
	result := RunStep{
		RunID: run.Run.RunID, Concurrency: run.concurrency, Requests: stats.counts,
		InputTokens: stats.input.bounds, OutputTokens: stats.output.bounds,
		SlotWaitMS: percentiles(stats.waits), TimeToFirstTokenMS: percentiles(stats.ttft),
		InterTokenLatencyMS: percentiles(stats.itl), Throughput: stats.throughput(),
		MemoryPeakMiB: map[string]*int64{},
	}
	if chat {
		result.ConcurrentStreamsPeak = new(streamPeak(stats.streams))
	}
	if !changed {
		result.PublicRequests = publicRequests(run.Run.RunID, observed.snapshots, stats.leases)
	}
	observeRun(&result, observed.snapshots)
	lengthMiss := stats.input.bounds != nil && stats.input.bounds.Min < run.workload.InputTokens
	if run.Run.OutputForced && stats.output.bounds != nil {
		want := *run.workload.OutputTokens
		lengthMiss = lengthMiss || stats.output.bounds.Min != want || stats.output.bounds.Max != want
	}
	settled := stats.counts.Completed + stats.counts.Failed
	result.Outcome, result.Reason = runOutcome(runEvidence{
		changed:      changed,
		timedOut:     stepTimedOut && settled < stats.counts.Requested,
		warmupReason: run.warmupReason,
		errorCode:    stats.firstError,
		lengthMiss:   lengthMiss,
		missingUsage: stats.input.count < stats.counts.Completed || chat && stats.output.count < stats.counts.Completed,
		queued:       result.ConcurrentStreamsPeak != nil && *result.ConcurrentStreamsPeak < result.Concurrency,
	})
	if chat && stats.output.count == stats.counts.Completed {
		return result, &stats.output.total
	}
	return result, nil
}

func observeRun(result *RunStep, snapshots []Snapshot) {
	var cpu observations
	for _, snapshot := range snapshots {
		run := snapshot.Runs[result.RunID]
		cpu.add(run.CPUPct)
		result.Engine.Running = peak(result.Engine.Running, run.Running)
		result.Engine.Waiting = peak(result.Engine.Waiting, run.Waiting)
		result.Engine.KVCacheUsage = peak(result.Engine.KVCacheUsage, run.KVCacheUsage)
		for _, pool := range snapshot.Pools {
			var memory *int64
			switch pool.Kind {
			case "gpu":
				memory = gpuMemory(pool, func(id string) bool { return id == result.RunID })
			case "host":
				memory = pool.HostMemoryMiB[result.RunID]
			}
			result.MemoryPeakMiB[pool.ID] = peak(result.MemoryPeakMiB[pool.ID], memory)
		}
	}
	result.CPUPct = cpu.summary()
}

func publicRequests(runID string, snapshots []Snapshot, leases uint64) *uint64 {
	if len(snapshots) < 2 {
		return nil
	}
	first := snapshots[0].Runs[runID].RequestsAdmitted
	last := snapshots[len(snapshots)-1].Runs[runID].RequestsAdmitted
	if first == nil || last == nil || *last < *first || *last-*first < leases {
		return nil
	}
	return new(*last - *first - leases)
}

func changedRuns(snapshots []Snapshot, previous map[string]RunSample, targets map[string]Target) map[string]bool {
	changed := make(map[string]bool, len(previous))
	for id, last := range previous {
		if target, ok := targets[id]; ok && last.TargetRevision != target.TargetRevision {
			changed[id] = true
		}
	}
	for _, snapshot := range snapshots {
		for id, run := range snapshot.Runs {
			if target, ok := targets[id]; ok {
				last := previous[id].RequestsAdmitted
				count := run.RequestsAdmitted
				if run.TargetRevision != target.TargetRevision || (last != nil && count != nil && *count < *last) {
					changed[id] = true
				}
				if count == nil {
					run.RequestsAdmitted = last
				}
				previous[id] = run
			}
		}
	}
	return changed
}

func summarizeStep(index int, window time.Duration, runs []stepRun, observed stepObservations, previous map[string]RunSample, timedOut bool, targets map[string]Target) StepReport {
	report := StepReport{Index: index, DurationMS: ms(window), Runs: []RunStep{}, Resources: stepPools(observed.snapshots, targets)}
	changed := changedRuns(observed.snapshots, previous, targets)
	totalOutput := 0.0
	hasOutput, knownOutput := false, true
	for _, run := range runs {
		revision, read := observed.revisions[run.Run.RunID]
		didChange := changed[run.Run.RunID] || (read && revision != run.TargetRevision)
		result, output := summarizeRun(run, observed, didChange, timedOut)
		report.Runs = append(report.Runs, result)
		if run.Run.Endpoint == Chat {
			hasOutput = true
			if output == nil {
				knownOutput = false
			} else {
				totalOutput += *output
			}
		}
	}
	if hasOutput && knownOutput {
		report.Throughput.OutputTokensPerSecond = rate(totalOutput, window)
	}
	report.Outcome, report.Reason = stepOutcome(report.Runs)
	return report
}

// DescribePools projects the sampled pool identity and holder attribution.
func DescribePools(pools []PoolSample, targets map[string]Target) []PoolReport {
	report := make([]PoolReport, len(pools))
	for i, pool := range pools {
		result := PoolReport{ID: pool.ID, Kind: pool.Kind, TotalMiB: pool.TotalMiB}
		if pool.Kind == "gpu" {
			result.GPUIdentity = &GPUIdentity{Vendor: pool.Vendor, Ownership: new(gpuOwnership(pool, targets)), Holders: slices.Clone(pool.Holders)}
		}
		report[i] = result
	}
	return report
}

func gpuOwnership(pool PoolSample, targets map[string]Target) string {
	if pool.Holders == nil {
		return "unknown"
	}
	ownership := "verified"
	for _, holder := range pool.Holders {
		if holder.MemoryMiB == nil {
			return "unknown"
		}
		if _, listed := targets[holderRunID(holder)]; !listed && len(holder.ListedRuns) == 0 {
			ownership = "shared"
		}
	}
	return ownership
}

func holderRunID(h Holder) string {
	if h.RunID == nil {
		return ""
	}
	return *h.RunID
}

func gpuMemory(pool PoolSample, include func(string) bool) *int64 {
	if pool.Holders == nil {
		return nil
	}
	var sum int64
	for _, h := range pool.Holders {
		matches := include(holderRunID(h))
		if len(h.ListedRuns) != 0 {
			matches = slices.ContainsFunc(h.ListedRuns, include)
		}
		if matches {
			if h.MemoryMiB == nil {
				return nil
			}
			sum += *h.MemoryMiB
		}
	}
	return &sum
}

type poolStats struct {
	report             PoolStep
	utilization, power observations
	cpu                observations
}

func (s *poolStats) observe(pool PoolSample, targets map[string]Target) {
	if pool.Kind == "gpu" {
		if s.report.GPUStats == nil {
			s.report.GPUStats = &GPUStats{}
		}
		gpu := s.report.GPUStats
		gpu.PeakUsedMiB = peak(gpu.PeakUsedMiB, pool.UsedMiB)
		gpu.MinimumFreeMiB = minimum(gpu.MinimumFreeMiB, pool.FreeMiB)
		other := gpuMemory(pool, func(id string) bool { _, listed := targets[id]; return !listed })
		gpu.OtherHoldersPeakMiB = peak(gpu.OtherHoldersPeakMiB, other)
		s.utilization.add(pool.UtilizationPct)
		s.power.add(pool.PowerW)
	} else {
		if s.report.SystemStats == nil {
			s.report.SystemStats = &SystemStats{}
		}
		system := s.report.SystemStats
		system.MinimumAvailableMiB = minimum(system.MinimumAvailableMiB, pool.AvailableMiB)
		system.PeakSwapUsedMiB = peak(system.PeakSwapUsedMiB, pool.SwapUsedMiB)
		s.cpu.add(pool.CPUPct)
	}
}

func stepPools(snapshots []Snapshot, targets map[string]Target) []PoolStep {
	pools := map[string]*poolStats{}
	var order []*poolStats
	for _, snapshot := range snapshots {
		for _, pool := range snapshot.Pools {
			stats := pools[pool.ID]
			if stats == nil {
				stats = &poolStats{report: PoolStep{ID: pool.ID}}
				pools[pool.ID] = stats
				order = append(order, stats)
			}
			stats.observe(pool, targets)
		}
	}
	report := make([]PoolStep, len(order))
	for i, stats := range order {
		if gpu := stats.report.GPUStats; gpu != nil {
			gpu.UtilizationPct, gpu.PowerW = stats.utilization.summary(), stats.power.summary()
		} else {
			stats.report.SystemStats.CPUPct = stats.cpu.summary()
		}
		report[i] = stats.report
	}
	return report
}
