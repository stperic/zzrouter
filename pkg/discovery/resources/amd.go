package resources

import (
	"bytes"
	"context"
	"regexp"
	"strings"
)

func liveAMD(ctx context.Context, query func(context.Context, ...string) ([]byte, error)) GPUStats {
	var m GPUStats

	idOut, err := query(ctx, "--showid")
	if err != nil || len(idOut) == 0 {
		return m
	}
	m.Count = countRocmSMIIDs(idOut)
	if m.Count == 0 {
		return m
	}

	if memOut, _ := query(ctx, "--showmeminfo", "vram"); len(memOut) > 0 {
		m.TotalMiB, m.UsedMiB = parseRocmSMIMemory(memOut)
		if m.TotalMiB != nil && m.UsedMiB != nil {
			m.FreeMiB = new(max(0, *m.TotalMiB-*m.UsedMiB))
		}
	}
	if useOut, _ := query(ctx, "--showuse"); len(useOut) > 0 {
		m.UtilizationPct = parseRocmSMIUtilization(useOut)
	}

	return m
}

// countRocmSMIIDs skips spacing between ROCm device rows.
func countRocmSMIIDs(out []byte) int {
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// ROCm 5 and 6 use different units and percent placements.
var (
	rocmSMIMemoryTotalValue = regexp.MustCompile(`(?i)total[^\d]*(\d+)`)
	rocmSMIMemoryUsedValue  = regexp.MustCompile(`(?i)used[^\d]*(\d+)`)
	rocmSMIUtilValue        = regexp.MustCompile(`(?i)gpu[^\n]*?use[^\d\n]*(\d+(?:\.\d+)?)\s*%?`)
)

// parseRocmSMIMemory sums rows using the reported unit even for small readings.
func parseRocmSMIMemory(out []byte) (total, used *int64) {
	isBytes := bytes.Contains(bytes.ToLower(out), []byte("(b):"))
	sum := func(pattern *regexp.Regexp) *int64 {
		var total *int64
		for _, match := range pattern.FindAllSubmatch(out, -1) {
			amount := quantity(string(match[1]))
			if amount == nil {
				continue
			}
			if isBytes {
				*amount /= 1 << 20
			}
			if total == nil {
				total = amount
			} else {
				*total += *amount
			}
		}
		return total
	}
	return sum(rocmSMIMemoryTotalValue), sum(rocmSMIMemoryUsedValue)
}

func parseRocmSMIUtilization(out []byte) *float64 {
	matches := rocmSMIUtilValue.FindAllSubmatch(out, -1)
	var sum float64
	var samples int
	for _, m := range matches {
		if value := reading(string(m[1])); value != nil {
			sum += *value
			samples++
		}
	}
	if samples == 0 {
		return nil
	}
	return new(sum / float64(samples))
}
