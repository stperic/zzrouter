package gpu

import (
	"context"
	"errors"
	"testing"
)

func TestLiveCardMetricsContext(t *testing.T) {
	t.Run("per-card rows carry their own join keys", func(t *testing.T) {
		opts := liveMetricsOptions{
			queryNVIDIASMI: stubNVIDIASMI([]nvidiaRow{
				{
					nvidiaFieldUUID:           "GPU-aaa",
					nvidiaFieldPCIBusID:       "00000000:41:00.0",
					nvidiaFieldMemoryTotalMiB: "97887",
					nvidiaFieldMemoryUsedMiB:  "30012",
					nvidiaFieldMemoryFreeMiB:  "67875",
					nvidiaFieldUtilizationGPU: "42",
				},
				{
					nvidiaFieldUUID:           "GPU-bbb",
					nvidiaFieldPCIBusID:       "0000:c1:00.0",
					nvidiaFieldMemoryTotalMiB: "97887",
					nvidiaFieldMemoryUsedMiB:  "0",
					nvidiaFieldMemoryFreeMiB:  "97887",
					nvidiaFieldUtilizationGPU: "0",
				},
			}, nil),
		}

		cards := liveCardMetricsContext(context.Background(), opts.withDefaults())
		if len(cards) != 2 {
			t.Fatalf("expected 2 cards, got %d", len(cards))
		}
		if cards[0].UUID != "GPU-aaa" || cards[0].FreeMemoryMiB != 67875 || cards[0].UsedMemoryMiB != 30012 {
			t.Errorf("first card mismatched: %+v", cards[0])
		}
		if cards[0].PCIAddress != "0000:41:00.0" {
			t.Errorf("PCI address must be canonical, got %q", cards[0].PCIAddress)
		}
		if cards[0].UtilizationPct != 42 {
			t.Errorf("utilization is per card, not averaged: %+v", cards[0])
		}
		if cards[1].FreeMemoryMiB != 97887 {
			t.Errorf("second card mismatched: %+v", cards[1])
		}
	})

	t.Run("an unmeasurable node returns nil, never a zeroed row", func(t *testing.T) {
		for name, opts := range map[string]liveMetricsOptions{
			"no nvidia-smi": {queryNVIDIASMI: stubNVIDIASMI(nil, nil)},
			"smi failed":    {queryNVIDIASMI: stubNVIDIASMI(nil, errors.New("driver mismatch"))},
			"no figures": {queryNVIDIASMI: stubNVIDIASMI([]nvidiaRow{{
				nvidiaFieldUUID:           "GPU-aaa",
				nvidiaFieldMemoryTotalMiB: "[N/A]",
				nvidiaFieldMemoryFreeMiB:  "[N/A]",
			}}, nil)},
		} {
			if cards := liveCardMetricsContext(context.Background(), opts.withDefaults()); cards != nil {
				t.Errorf("%s: expected nil so callers render unknown, got %+v", name, cards)
			}
		}
	})
}

func TestCardMetricsIndex(t *testing.T) {
	// nvidia-smi orders rows by NVML, so the second card's metrics arrive
	// first here. A positional join would report card A's memory as B's.
	idx := NewCardMetricsIndex([]CardMetrics{
		{UUID: "GPU-bbb", PCIAddress: "0000:c1:00.0", FreeMemoryMiB: 97887},
		{UUID: "GPU-aaa", PCIAddress: "0000:41:00.0", FreeMemoryMiB: 67875, UsedMemoryMiB: 30012},
	})

	t.Run("joins on identity, not position", func(t *testing.T) {
		got, ok := idx.ForCard(Card{UUID: "GPU-aaa", PCIAddress: "0000:41:00.0"})
		if !ok || got.FreeMemoryMiB != 67875 || got.UsedMemoryMiB != 30012 {
			t.Errorf("got %+v (ok=%v), want card aaa's figures", got, ok)
		}
	})

	t.Run("falls back to the PCI slot when a card has no UUID", func(t *testing.T) {
		if got, ok := idx.ForCard(Card{PCIAddress: "0000:c1:00.0"}); !ok || got.UUID != "GPU-bbb" {
			t.Errorf("got %+v (ok=%v), want card bbb", got, ok)
		}
	})

	t.Run("normalizes the PCI slot before matching", func(t *testing.T) {
		if _, ok := idx.ForCard(Card{PCIAddress: "00000000:41:00.0"}); !ok {
			t.Error("an 8-digit domain must match the canonical form")
		}
	})

	t.Run("an unmeasured card misses", func(t *testing.T) {
		if _, ok := NewCardMetricsIndex(nil).ForCard(Card{UUID: "GPU-aaa"}); ok {
			t.Error("no metrics must read as unknown, not as zero free")
		}
		if _, ok := idx.ForCard(Card{Name: "Apple M5 Max"}); ok {
			t.Error("a card with no identity keys can't be joined")
		}
	})
}
