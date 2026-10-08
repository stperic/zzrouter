package gpu

import (
	"bytes"
	"context"
	"regexp"
	"strconv"
	"strings"
)

// LiveMetrics is per-vendor aggregate runtime metrics pulled from
// nvidia-smi / rocm-smi at poll time. Memory figures are in MiB
// (consistent with Card.MemoryMiB); UtilizationPct is 0-100,
// averaged across every card that reported a meaningful value.
//
// Count == 0 means the vendor's SMI tool was absent OR failed to
// produce usable rows. LiveMetrics intentionally does NOT
// distinguish those two cases — callers that need to tell "no
// hardware" from "broken driver" should consult Detection.State
// via InventoryContext, which is served by a separate static-
// detection code path in this package.
//
// LiveMetricsSet bundles NVIDIA and AMD into one value because
// those are the only two vendors with an SMI-shaped live-metrics
// story in this codebase: Intel has no equivalent tool, and
// Apple Silicon's "GPU memory" is a share of unified system RAM
// that lives in a layer with a gopsutil dep (pkg/cluster/resource_tracker
// still owns the Apple path). A future fifth vendor would be a
// one-line struct addition and every existing caller would keep
// compiling.
type LiveMetrics struct {
	Vendor         Vendor
	Count          int
	TotalMemoryMiB int64
	UsedMemoryMiB  int64
	FreeMemoryMiB  int64
	UtilizationPct float64
}

// LiveMetricsSet is the full runtime-metrics picture for one poll.
// On a mixed NVIDIA + AMD node both fields are populated; on a
// single-vendor box the other slot is a zero-value LiveMetrics
// with Count == 0.
type LiveMetricsSet struct {
	NVIDIA LiveMetrics
	AMD    LiveMetrics
}

// liveMetricsOptions threads the subprocess seams the live-metrics
// path needs through one struct, matching the inventoryOptions
// pattern. Production callers use defaults; tests stub
// queryNVIDIASMI and queryRocmSMI to pin output.
type liveMetricsOptions struct {
	queryNVIDIASMI func(ctx context.Context, fields ...string) ([]nvidiaRow, error)
	queryRocmSMI   func(ctx context.Context, args ...string) ([]byte, error)
}

func (o liveMetricsOptions) withDefaults() liveMetricsOptions {
	if o.queryNVIDIASMI == nil {
		o.queryNVIDIASMI = queryNVIDIASMI
	}
	if o.queryRocmSMI == nil {
		o.queryRocmSMI = queryRocmSMI
	}
	return o
}

// LiveMetricsContext polls live GPU metrics for one scrape. It
// issues at most one subprocess per vendor per call — no ghw,
// no sysfs walk, no registry read — so it is safe to invoke on
// a high-frequency polling interval. On a no-GPU box both SMI
// binaries will be missing, each query helper returns (nil, nil),
// and the result is a zero LiveMetricsSet.
func LiveMetricsContext(ctx context.Context) LiveMetricsSet {
	return liveMetricsContext(ctx, liveMetricsOptions{}.withDefaults())
}

// liveMetricsContext is the test-injection seam. Production
// callers go through LiveMetricsContext which supplies the
// production query helpers; tests pass a partial options struct.
func liveMetricsContext(ctx context.Context, opts liveMetricsOptions) LiveMetricsSet {
	return LiveMetricsSet{
		NVIDIA: liveNVIDIAMetrics(ctx, opts.queryNVIDIASMI),
		AMD:    liveAMDMetrics(ctx, opts.queryRocmSMI),
	}
}

// liveNVIDIAMetrics aggregates nvidia-smi --query-gpu rows into
// a single LiveMetrics. Memory fields sum across cards.
//
// Utilization is averaged only over cards that reported a
// meaningful value — rows where nvidia-smi returns `[N/A]` (some
// A100 SKUs, older Quadros) are silently dropped from both the
// numerator and the denominator. The pre-refactor implementation
// in resource_tracker.go divided totalUtilization by len(rows)
// unconditionally, which pulled the average toward zero whenever
// any card failed to report utilization.
func liveNVIDIAMetrics(ctx context.Context, query func(context.Context, ...string) ([]nvidiaRow, error)) LiveMetrics {
	rows, err := query(ctx,
		nvidiaFieldMemoryTotalMiB,
		nvidiaFieldMemoryUsedMiB,
		nvidiaFieldMemoryFreeMiB,
		nvidiaFieldUtilizationGPU,
	)
	if err != nil || len(rows) == 0 {
		return LiveMetrics{Vendor: VendorNVIDIA}
	}
	m := LiveMetrics{Vendor: VendorNVIDIA, Count: len(rows)}

	var utilSum float64
	var utilSamples int
	for _, row := range rows {
		if v := row[nvidiaFieldMemoryTotalMiB]; isMeaningful(v) {
			if n, perr := strconv.ParseInt(v, 10, 64); perr == nil {
				m.TotalMemoryMiB += n
			}
		}
		if v := row[nvidiaFieldMemoryUsedMiB]; isMeaningful(v) {
			if n, perr := strconv.ParseInt(v, 10, 64); perr == nil {
				m.UsedMemoryMiB += n
			}
		}
		if v := row[nvidiaFieldMemoryFreeMiB]; isMeaningful(v) {
			if n, perr := strconv.ParseInt(v, 10, 64); perr == nil {
				m.FreeMemoryMiB += n
			}
		}
		if v := row[nvidiaFieldUtilizationGPU]; isMeaningful(v) {
			if n, perr := strconv.ParseFloat(v, 64); perr == nil {
				utilSum += n
				utilSamples++
			}
		}
	}
	if utilSamples > 0 {
		m.UtilizationPct = utilSum / float64(utilSamples)
	}
	return m
}

// liveAMDMetrics issues the three rocm-smi calls the AMD live-
// metrics path needs (--showid, --showmeminfo vram, --showuse)
// and assembles the result.
//
// The memory parser handles both ROCm 5.x (`Total Memory (MB):
// 16384`) and 6.x (`VRAM Total Memory (B): 17163091968`) formats
// via the same magnitude inference that parseRocmSMIVRAMMiB uses
// — any value ≥ 1 GiB is treated as bytes, smaller values are
// already MiB. The pre-refactor implementation in
// resource_tracker.parseAMDMemoryOutput hardcoded bytes → MiB
// with `bytes / (1024 * 1024)`, silently mis-reporting memory
// on ROCm 5.x nodes (a 16 GB card would register as 16 / 1048576
// ≈ 0 MiB because 16384 was interpreted as a byte count).
func liveAMDMetrics(ctx context.Context, query func(context.Context, ...string) ([]byte, error)) LiveMetrics {
	m := LiveMetrics{Vendor: VendorAMD}

	idOut, err := query(ctx, "--showid")
	if err != nil || len(idOut) == 0 {
		return m
	}
	m.Count = countRocmSMIIDs(idOut)
	if m.Count == 0 {
		return m
	}

	if memOut, _ := query(ctx, "--showmeminfo", "vram"); len(memOut) > 0 {
		total, used := parseRocmSMIMemory(memOut)
		m.TotalMemoryMiB = total
		m.UsedMemoryMiB = used
		if total > used {
			m.FreeMemoryMiB = total - used
		}
	}
	if useOut, _ := query(ctx, "--showuse"); len(useOut) > 0 {
		m.UtilizationPct = parseRocmSMIUtilization(useOut)
	}

	return m
}

// countRocmSMIIDs counts the non-empty lines in `rocm-smi --showid`
// output. Each GPU is one line; blank separators and the banner
// header are ignored.
func countRocmSMIIDs(out []byte) int {
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// rocmSMIMemoryTotalValue / rocmSMIMemoryUsedValue / rocmSMIUtilValue
// are keyword-anchored parsers for the three rocm-smi outputs the
// AMD live-metrics path consumes. Each one is rooted on a label
// that precedes the number so, e.g., "Total Memory (B): 17163091968"
// and "Used Memory (B): 0" don't leak into each other's capture.
//
// Utilization is optionally followed by "%"; ROCm 5.x tends to
// emit `GPU use (%): 25` (percent sign before the number as part
// of the label), ROCm 6.x sometimes emits `25%` with the sign
// trailing. The regex permits both because the `\s*%?` tail at
// the end makes the percent sign optional.
var (
	rocmSMIMemoryTotalValue = regexp.MustCompile(`(?i)total[^\d]*(\d+)`)
	rocmSMIMemoryUsedValue  = regexp.MustCompile(`(?i)used[^\d]*(\d+)`)
	rocmSMIUtilValue        = regexp.MustCompile(`(?i)gpu[^\n]*?use[^\d\n]*(\d+(?:\.\d+)?)\s*%?`)
)

// parseRocmSMIMemory extracts total and used memory (both in
// MiB) from --showmeminfo vram output, summing across every card
// the output reports. rocm-smi emits one Total and one Used line
// per GPU, so on a two-card box we want FindAllSubmatch / sum,
// not FindSubmatch / first-match — the latter would report
// GPU[0]'s figures alongside a Count of 2 and the caller would
// silently see wrong aggregates.
//
// Format detection is by string match rather than magnitude:
// ROCm 6.x emits the label "(B):" (bytes), ROCm 5.x emits "(MB):"
// or no unit label, so we scan for the lowercase "(b):" substring
// once and apply the resulting unit to every numeric capture
// consistently. A magnitude-based heuristic would mis-classify
// small *used* values — a 512 MiB used on a 6.x card is
// 536870912 bytes, well below 1 GiB, and would be interpreted as
// "already MiB". Total-only parsers like parseRocmSMIVRAMMiB can
// get away with the heuristic because total is always ≥ 1 GiB on
// real cards; used cannot.
func parseRocmSMIMemory(out []byte) (totalMiB, usedMiB int64) {
	isBytes := bytes.Contains(bytes.ToLower(out), []byte("(b):"))
	for _, m := range rocmSMIMemoryTotalValue.FindAllSubmatch(out, -1) {
		totalMiB += normalizeVRAMUnitsWithFormat(string(m[1]), isBytes)
	}
	for _, m := range rocmSMIMemoryUsedValue.FindAllSubmatch(out, -1) {
		usedMiB += normalizeVRAMUnitsWithFormat(string(m[1]), isBytes)
	}
	return
}

// normalizeVRAMUnitsWithFormat converts a raw rocm-smi numeric
// token to MiB given an explicit format flag. Returns 0 on
// parse failure or a non-positive value.
func normalizeVRAMUnitsWithFormat(s string, isBytes bool) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return 0
	}
	if isBytes {
		const bytesPerMiB int64 = 1 << 20
		return v / bytesPerMiB
	}
	return v
}

// normalizeVRAMUnits applies magnitude-based unit inference to
// a raw rocm-smi numeric token where only a total is in scope.
// A value ≥ 1 GiB (2^30) must be a byte count — ROCm 6.x — and
// gets divided by 1 MiB. Anything smaller is already MiB from
// ROCm 5.x and is returned verbatim. Returns 0 on parse failure.
//
// Safe for total-memory parsing because real GPUs have ≥ 1 GiB
// of VRAM. Unsafe for "used" memory, which can be any size;
// parseRocmSMIMemory uses string-based format detection instead.
func normalizeVRAMUnits(s string) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return 0
	}
	const oneGiB int64 = 1 << 30
	const bytesPerMiB int64 = 1 << 20
	if v >= oneGiB {
		return v / bytesPerMiB
	}
	return v
}

// parseRocmSMIUtilization extracts the GPU utilization
// percentage from --showuse output, averaged across every card
// that reported a parseable value. rocm-smi emits one line per
// GPU; FindAllSubmatch captures them all, the loop sums the
// samples, and the caller sees the mean. Missing or malformed
// rows are silently dropped from both the numerator and the
// denominator — same contract as liveNVIDIAMetrics so a card
// that reports "[N/A]" or fails to parse doesn't drag the
// average toward zero.
//
// Returns 0 on no matches or all-malformed samples; callers
// treat 0 as "unknown".
func parseRocmSMIUtilization(out []byte) float64 {
	matches := rocmSMIUtilValue.FindAllSubmatch(out, -1)
	if len(matches) == 0 {
		return 0
	}
	var sum float64
	var samples int
	for _, m := range matches {
		v, err := strconv.ParseFloat(string(m[1]), 64)
		if err != nil {
			continue
		}
		sum += v
		samples++
	}
	if samples == 0 {
		return 0
	}
	return sum / float64(samples)
}
