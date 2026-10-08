package gpu

import (
	"context"
	"strconv"
)

// CardMetrics is one physical card's runtime memory state at poll time.
// Memory figures are in MiB (consistent with Card.MemoryMiB) and
// UtilizationPct is 0-100.
//
// UUID and PCIAddress are the join keys back to a Card: nvidia-smi row
// order is not PCI order, so associating by slice index silently
// mislabels cards on a multi-GPU box.
type CardMetrics struct {
	UUID           string
	PCIAddress     string
	TotalMemoryMiB int64
	UsedMemoryMiB  int64
	FreeMemoryMiB  int64
	UtilizationPct float64
}

// LiveCardMetricsContext polls per-card runtime metrics for one scrape.
// LiveMetricsContext answers "how much VRAM does this node have free";
// this answers it per card, which is what any per-card display needs.
//
// NVIDIA only. rocm-smi reports per-card state in a different shape and
// no caller needs AMD per-card yet; an AMD node returns nil here and its
// callers must render "unknown" rather than zero. Same contract when
// nvidia-smi is absent or fails: nil, never a zeroed row, because "0 GB
// free" and "we could not measure" are opposite claims to an operator.
func LiveCardMetricsContext(ctx context.Context) []CardMetrics {
	return liveCardMetricsContext(ctx, liveMetricsOptions{}.withDefaults())
}

// liveCardMetricsContext is the test-injection seam, matching
// liveMetricsContext.
func liveCardMetricsContext(ctx context.Context, opts liveMetricsOptions) []CardMetrics {
	rows, err := opts.queryNVIDIASMI(ctx,
		nvidiaFieldUUID,
		nvidiaFieldPCIBusID,
		nvidiaFieldMemoryTotalMiB,
		nvidiaFieldMemoryUsedMiB,
		nvidiaFieldMemoryFreeMiB,
		nvidiaFieldUtilizationGPU,
	)
	if err != nil || len(rows) == 0 {
		return nil
	}

	out := make([]CardMetrics, 0, len(rows))
	for _, row := range rows {
		m := CardMetrics{
			PCIAddress:     normalizePCIAddress(row[nvidiaFieldPCIBusID]),
			TotalMemoryMiB: rowMiB(row, nvidiaFieldMemoryTotalMiB),
			UsedMemoryMiB:  rowMiB(row, nvidiaFieldMemoryUsedMiB),
			FreeMemoryMiB:  rowMiB(row, nvidiaFieldMemoryFreeMiB),
		}
		// isMeaningful keeps "[N/A]" from becoming a literal lookup key,
		// matching how the inventory path guards the same field.
		if v := row[nvidiaFieldUUID]; isMeaningful(v) {
			m.UUID = v
		}
		if v := row[nvidiaFieldUtilizationGPU]; isMeaningful(v) {
			if n, perr := strconv.ParseFloat(v, 64); perr == nil {
				m.UtilizationPct = n
			}
		}
		// A row with no memory figures at all carries nothing a caller
		// can use; dropping it keeps "present in the slice" equivalent
		// to "measured".
		if m.TotalMemoryMiB == 0 && m.UsedMemoryMiB == 0 && m.FreeMemoryMiB == 0 {
			continue
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func rowMiB(row nvidiaRow, field string) int64 {
	v := row[field]
	if !isMeaningful(v) {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// CardMetricsIndex joins live per-card metrics back onto detected cards.
// UUID is the durable key and PCI address the fallback for probes that
// report a slot but no UUID. Slice position is deliberately not a key:
// nvidia-smi orders rows by NVML, not by PCI or by detection order, so
// pairing by index quietly attributes one card's memory to another on a
// multi-GPU box.
//
// A miss means "this card was not measured this scrape", which callers
// must render as unknown rather than as zero free.
type CardMetricsIndex struct {
	byUUID map[string]CardMetrics
	byPCI  map[string]CardMetrics
}

// NewCardMetricsIndex builds the lookup. A nil or empty slice yields an
// index that misses every card, which is the correct answer for a node
// whose vendor tool couldn't report.
func NewCardMetricsIndex(metrics []CardMetrics) CardMetricsIndex {
	idx := CardMetricsIndex{
		byUUID: make(map[string]CardMetrics, len(metrics)),
		byPCI:  make(map[string]CardMetrics, len(metrics)),
	}
	for _, m := range metrics {
		if m.UUID != "" {
			idx.byUUID[m.UUID] = m
		}
		if m.PCIAddress != "" {
			idx.byPCI[m.PCIAddress] = m
		}
	}
	return idx
}

// ForCard returns the live metrics for a detected card.
func (i CardMetricsIndex) ForCard(card Card) (CardMetrics, bool) {
	return i.ForIdentity(card.UUID, card.PCIAddress)
}

// ForIdentity returns the live metrics for a card named by either durable
// identifier. Callers that hold a reshaped DTO rather than a Card (the
// host inventory path) use this directly.
func (i CardMetricsIndex) ForIdentity(uuid, pciAddress string) (CardMetrics, bool) {
	if uuid != "" {
		if m, ok := i.byUUID[uuid]; ok {
			return m, true
		}
	}
	if pciAddress != "" {
		if m, ok := i.byPCI[normalizePCIAddress(pciAddress)]; ok {
			return m, true
		}
	}
	return CardMetrics{}, false
}
