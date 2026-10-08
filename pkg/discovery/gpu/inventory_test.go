package gpu

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubRocmSMI returns a closure that answers rocm-smi calls by
// matching the argv tail against keys in responses. The stub is
// installed via inventoryOptions.queryRocmSMI; a missing key
// returns (nil, nil) which matches the production "binary not
// found" semantics.
func stubRocmSMI(responses map[string][]byte) func(context.Context, ...string) ([]byte, error) {
	return func(_ context.Context, args ...string) ([]byte, error) {
		key := strings.Join(args, " ")
		if body, ok := responses[key]; ok {
			return body, nil
		}
		for k, v := range responses {
			if strings.HasSuffix(key, k) {
				return v, nil
			}
		}
		return nil, nil
	}
}

// stubNVIDIASMI is the analogous nvidia-smi stub: ignore the field
// list and return the rows the caller wants. Field padding mirrors
// queryNVIDIASMI's contract so nvidiaCards sees the expected keys.
func stubNVIDIASMI(rows []nvidiaRow, err error) func(context.Context, ...string) ([]nvidiaRow, error) {
	return func(_ context.Context, fields ...string) ([]nvidiaRow, error) {
		out := make([]nvidiaRow, 0, len(rows))
		for _, r := range rows {
			row := make(nvidiaRow, len(fields))
			for _, f := range fields {
				row[f] = r[f]
			}
			out = append(out, row)
		}
		return out, err
	}
}

// stubProbe pins the per-vendor Detection InventoryContext sees,
// so the enrichment paths can be driven without a real platform
// probe touching the filesystem or registry.
func stubProbe(states map[Vendor]Detection) func(context.Context, Vendor) (Detection, error) {
	return func(_ context.Context, v Vendor) (Detection, error) {
		if d, ok := states[v]; ok {
			return d, nil
		}
		return Detection{Vendor: v, State: StateAbsent}, nil
	}
}

func TestParseRocmSMIVRAMMiB(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int64
	}{
		{
			name: "ROCm 5.x vram: 16384 MB",
			in:   "GPU[0]\t\t: vram Total Memory (MB): 16384\n",
			want: 16384,
		},
		{
			name: "ROCm 6.x bytes 17163091968 → 16376 MiB",
			in:   "GPU[0]\t\t: VRAM Total Memory (B): 17163091968\n",
			want: 17163091968 / (1 << 20),
		},
		{
			name: "ROCm 6.x Used Memory 0 → returns 0 (treated as unknown)",
			in:   "GPU[0]\t\t: VRAM Total Used Memory (B): 0\n",
			want: 0,
		},
		{
			name: "no vram token",
			in:   "GPU[0]\t\t: nothing relevant here\n",
			want: 0,
		},
		{
			name: "empty",
			in:   "",
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRocmSMIVRAMMiB([]byte(tc.in))
			if got != tc.want {
				t.Errorf("parseRocmSMIVRAMMiB = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestNvidiaCards(t *testing.T) {
	t.Run("happy path: two GPUs with full field set", func(t *testing.T) {
		rows := []nvidiaRow{
			{
				nvidiaFieldPCIBusID:       "00000000:01:00.0",
				nvidiaFieldName:           "NVIDIA GeForce RTX 4090",
				nvidiaFieldMemoryTotalMiB: "24564",
				nvidiaFieldComputeCap:     "8.9",
				nvidiaFieldDriverVersion:  "550.54.14",
			},
			{
				nvidiaFieldPCIBusID:       "0000:83:00.0",
				nvidiaFieldName:           "NVIDIA A100-SXM4-40GB",
				nvidiaFieldMemoryTotalMiB: "40960",
				nvidiaFieldComputeCap:     "8.0",
				nvidiaFieldDriverVersion:  "550.54.14",
			},
		}
		cards := nvidiaCards(context.Background(), stubNVIDIASMI(rows, nil))
		if len(cards) != 2 {
			t.Fatalf("got %d cards, want 2", len(cards))
		}
		// First card: PCI normalized from 8-digit → 4-digit domain.
		if cards[0].PCIAddress != "0000:01:00.0" {
			t.Errorf("card[0] PCIAddress = %q, want canonical 0000:01:00.0", cards[0].PCIAddress)
		}
		if cards[0].Name != "NVIDIA GeForce RTX 4090" {
			t.Errorf("card[0] Name = %q", cards[0].Name)
		}
		if cards[0].MemoryMiB != 24564 {
			t.Errorf("card[0] MemoryMiB = %d", cards[0].MemoryMiB)
		}
		if cards[0].ComputeCapability != "89" {
			t.Errorf("card[0] ComputeCapability = %q, want 89 (dot-stripped)", cards[0].ComputeCapability)
		}
		if cards[0].DriverVersion != "550.54.14" {
			t.Errorf("card[0] DriverVersion = %q", cards[0].DriverVersion)
		}
		// Second card: 40 GB A100.
		if cards[1].MemoryMiB != 40960 || cards[1].ComputeCapability != "80" {
			t.Errorf("card[1] = %+v", cards[1])
		}
	})

	t.Run("populates uuid + driver_index for runtime affinity", func(t *testing.T) {
		// CUDA_VISIBLE_DEVICES accepts both UUIDs and integer indices.
		// UUID is the durable cross-reboot pin; driver_index is what
		// nvidia-smi prints. zzrouter exposes both so callers pick the
		// right level of stability for their use case.
		rows := []nvidiaRow{
			{
				nvidiaFieldPCIBusID: "0000:01:00.0",
				nvidiaFieldName:     "NVIDIA A16-8Q",
				nvidiaFieldUUID:     "GPU-d2c30f4e-5b2c-411f-aa7f-9b29c1d2e3f4",
				nvidiaFieldIndex:    "0",
			},
		}
		cards := nvidiaCards(context.Background(), stubNVIDIASMI(rows, nil))
		if len(cards) != 1 {
			t.Fatalf("got %d cards, want 1", len(cards))
		}
		if cards[0].UUID != "GPU-d2c30f4e-5b2c-411f-aa7f-9b29c1d2e3f4" {
			t.Errorf("UUID = %q", cards[0].UUID)
		}
		if cards[0].DriverIndex == nil || *cards[0].DriverIndex != 0 {
			t.Errorf("DriverIndex = %v, want pointer to 0", cards[0].DriverIndex)
		}
	})

	t.Run("missing uuid + index leave fields zero/nil", func(t *testing.T) {
		// Older driver versions or trimmed query sets must not panic.
		// DriverIndex is *int so absent → nil (distinct from "GPU 0").
		rows := []nvidiaRow{
			{
				nvidiaFieldPCIBusID: "0000:01:00.0",
				nvidiaFieldName:     "NVIDIA Older Card",
			},
		}
		cards := nvidiaCards(context.Background(), stubNVIDIASMI(rows, nil))
		if len(cards) != 1 {
			t.Fatalf("got %d cards, want 1", len(cards))
		}
		if cards[0].UUID != "" {
			t.Errorf("UUID should be empty when not in row, got %q", cards[0].UUID)
		}
		if cards[0].DriverIndex != nil {
			t.Errorf("DriverIndex should be nil when not in row, got %v", *cards[0].DriverIndex)
		}
	})

	t.Run("nvidia-smi error → nil cards", func(t *testing.T) {
		cards := nvidiaCards(context.Background(), stubNVIDIASMI(nil, errors.New("boom")))
		if cards != nil {
			t.Errorf("expected nil on error, got %+v", cards)
		}
	})

	t.Run("nvidia-smi absent (nil, nil) → nil cards", func(t *testing.T) {
		cards := nvidiaCards(context.Background(), stubNVIDIASMI(nil, nil))
		if cards != nil {
			t.Errorf("expected nil when binary missing, got %+v", cards)
		}
	})

	t.Run("[N/A] fields are skipped", func(t *testing.T) {
		rows := []nvidiaRow{{
			nvidiaFieldPCIBusID:       "0000:01:00.0",
			nvidiaFieldName:           "NVIDIA T4",
			nvidiaFieldMemoryTotalMiB: "[N/A]",
			nvidiaFieldComputeCap:     "[N/A]",
			nvidiaFieldDriverVersion:  "",
		}}
		cards := nvidiaCards(context.Background(), stubNVIDIASMI(rows, nil))
		if len(cards) != 1 {
			t.Fatalf("got %d", len(cards))
		}
		if cards[0].MemoryMiB != 0 || cards[0].ComputeCapability != "" || cards[0].DriverVersion != "" {
			t.Errorf("[N/A] should land as zero values, got %+v", cards[0])
		}
		if cards[0].Name != "NVIDIA T4" {
			t.Errorf("Name should still populate, got %q", cards[0].Name)
		}
	})
}

func TestWithCachedInventory(t *testing.T) {
	// WithCachedInventory must satisfy two invariants so the
	// install flow's preflight + plan-building don't probe twice:
	//   1. Repeated InventoryContext calls on the wrapped ctx hit
	//      the cache, not the real subprocess seam.
	//   2. ProbeContext on the wrapped ctx is served from the same
	//      cached Inventory.
	pre := &Inventory{
		Vendors: map[Vendor]Detection{
			VendorNVIDIA: {Vendor: VendorNVIDIA, State: StateDriverOK, Count: 2, Name: "RTX 4090"},
			VendorAMD:    {Vendor: VendorAMD, State: StateAbsent},
			VendorApple:  {Vendor: VendorApple, State: StateAbsent},
		},
		Cards: []Card{
			{Vendor: VendorNVIDIA, Name: "RTX 4090", CUDAVersion: "12.4"},
			{Vendor: VendorNVIDIA, Name: "RTX 4090", CUDAVersion: "12.4"},
		},
	}
	ctx := context.WithValue(context.Background(), inventoryCacheKey{}, pre)

	t.Run("InventoryContext returns cached inventory", func(t *testing.T) {
		got := InventoryContext(ctx)
		if len(got.Cards) != 2 || got.Cards[0].CUDAVersion != "12.4" {
			t.Errorf("expected cached 2 cards with CUDA 12.4, got %+v", got.Cards)
		}
	})

	t.Run("ProbeContext returns cached Detection", func(t *testing.T) {
		det, err := ProbeContext(ctx, VendorNVIDIA)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if det.State != StateDriverOK || det.Name != "RTX 4090" {
			t.Errorf("expected cached DriverOK RTX 4090, got %+v", det)
		}
	})

	t.Run("WithCachedInventory is idempotent", func(t *testing.T) {
		// Wrapping an already-wrapped ctx must not re-probe.
		again := WithCachedInventory(ctx)
		inv2, ok := cachedInventory(again)
		if !ok || inv2 != pre {
			t.Errorf("expected idempotent wrap preserving cache identity, got %v ok=%v", inv2, ok)
		}
	})

	t.Run("no cache means no ctx value", func(t *testing.T) {
		if _, ok := cachedInventory(context.Background()); ok {
			t.Errorf("fresh ctx must not report a cached inventory")
		}
	})
}

func TestAmdCards(t *testing.T) {
	t.Run("two GPUs, rocm-smi enumerates in reverse PCI order — memory follows bus", func(t *testing.T) {
		// rocm-smi indexes: 0 = 83:00.0 (32 GB), 1 = 03:00.0 (16 GB).
		// A broken index join would put 32 GB on the wrong card.
		query := stubRocmSMI(map[string][]byte{
			"--showbus":               []byte("GPU[0]\t\t: 0000:83:00.0\nGPU[1]\t\t: 0000:03:00.0\n"),
			"--showdriverversion":     []byte("6.1.0-1234\n"),
			"-i 0 --showmeminfo vram": []byte("GPU[0]\t\t: VRAM Total Memory (B): 34326183936\n"), // ~32 GB
			"-i 1 --showmeminfo vram": []byte("GPU[1]\t\t: vram Total Memory (MB): 16384\n"),      // 16 GB
		})
		cards := amdCards(context.Background(), query)
		if len(cards) != 2 {
			t.Fatalf("got %d cards, want 2", len(cards))
		}
		// In rocm-smi enumeration order, so [0] is 83:00.0 (32 GB).
		if cards[0].PCIAddress != "0000:83:00.0" {
			t.Errorf("cards[0] PCIAddress = %q", cards[0].PCIAddress)
		}
		wantMiB := int64(34326183936) / (1 << 20)
		if cards[0].MemoryMiB != wantMiB {
			t.Errorf("cards[0] MemoryMiB = %d, want %d (would land on wrong card with index join)", cards[0].MemoryMiB, wantMiB)
		}
		if cards[1].PCIAddress != "0000:03:00.0" || cards[1].MemoryMiB != 16384 {
			t.Errorf("cards[1] = %+v", cards[1])
		}
		// Driver version is the same on both cards.
		if cards[0].DriverVersion != "6.1.0-1234" || cards[1].DriverVersion != "6.1.0-1234" {
			t.Errorf("driver version missing: %+v / %+v", cards[0], cards[1])
		}
		// rocm-smi index is the integer HIP_VISIBLE_DEVICES consumes.
		// Mirrors what nvidia-smi --query-gpu=index gives for NVIDIA.
		if cards[0].DriverIndex == nil || *cards[0].DriverIndex != 0 {
			t.Errorf("cards[0].DriverIndex = %v, want pointer to 0", cards[0].DriverIndex)
		}
		if cards[1].DriverIndex == nil || *cards[1].DriverIndex != 1 {
			t.Errorf("cards[1].DriverIndex = %v, want pointer to 1", cards[1].DriverIndex)
		}
	})

	t.Run("populates uuid from rocm-smi --showuniqueid", func(t *testing.T) {
		// The 16-hex Unique ID is stable across reboots; matches the
		// NVIDIA UUID role in the Card.UUID field.
		query := stubRocmSMI(map[string][]byte{
			"--showbus":               []byte("GPU[0]\t\t: 0000:03:00.0\n"),
			"--showdriverversion":     []byte("6.1.0-1234\n"),
			"-i 0 --showmeminfo vram": []byte("GPU[0]\t\t: VRAM Total Memory (B): 34326183936\n"),
			"-i 0 --showuniqueid":     []byte("GPU[0]\t\t: Unique ID: 0x1234567890abcdef\n"),
		})
		cards := amdCards(context.Background(), query)
		if len(cards) != 1 {
			t.Fatalf("got %d cards, want 1", len(cards))
		}
		if cards[0].UUID != "1234567890abcdef" {
			t.Errorf("UUID = %q, want 1234567890abcdef (lowercased, 0x stripped)", cards[0].UUID)
		}
	})

	t.Run("missing --showuniqueid leaves uuid empty (older ROCm)", func(t *testing.T) {
		// ROCm < 5.x doesn't expose unique-id; absence must not break
		// the rest of the card population.
		query := stubRocmSMI(map[string][]byte{
			"--showbus":               []byte("GPU[0]\t\t: 0000:03:00.0\n"),
			"--showdriverversion":     []byte("4.5.0\n"),
			"-i 0 --showmeminfo vram": []byte("GPU[0]\t\t: vram Total Memory (MB): 16384\n"),
		})
		cards := amdCards(context.Background(), query)
		if len(cards) != 1 {
			t.Fatalf("got %d cards, want 1", len(cards))
		}
		if cards[0].UUID != "" {
			t.Errorf("UUID should be empty when --showuniqueid not stubbed, got %q", cards[0].UUID)
		}
		// The rest of the card should still be populated.
		if cards[0].MemoryMiB != 16384 || cards[0].PCIAddress != "0000:03:00.0" {
			t.Errorf("missing uuid broke other fields: %+v", cards[0])
		}
	})

	t.Run("parseRocmSMIUniqueID happy + edge cases", func(t *testing.T) {
		cases := []struct {
			name string
			out  []byte
			want string
		}{
			{"standard 16-hex", []byte("GPU[0]\t\t: Unique ID: 0x1234567890abcdef\n"), "1234567890abcdef"},
			{"upper-case hex", []byte("GPU[0]	: Unique ID: 0xDEADBEEFCAFEBABE\n"), "deadbeefcafebabe"},
			{"no 0x prefix", []byte("GPU[0]	: Unique ID: 1234567890abcdef\n"), "1234567890abcdef"},
			{"mixed prefix line", []byte("Some other line\nGPU[2]	: Unique ID: 0xABCDEF\nMore lines\n"), "abcdef"},
			{"empty", nil, ""},
			{"unrelated output", []byte("nothing here\n"), ""},
		}
		for _, tc := range cases {
			got := parseRocmSMIUniqueID(tc.out)
			if got != tc.want {
				t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
			}
		}
	})

	t.Run("empty --showbus → nil cards", func(t *testing.T) {
		cards := amdCards(context.Background(), stubRocmSMI(map[string][]byte{
			"--showbus": []byte(""),
		}))
		if cards != nil {
			t.Errorf("expected nil on empty --showbus, got %+v", cards)
		}
	})

	t.Run("malformed --showbus (no bus rows) → nil cards", func(t *testing.T) {
		cards := amdCards(context.Background(), stubRocmSMI(map[string][]byte{
			"--showbus": []byte("========= ROCm SMI =========\nno bus lines\n"),
		}))
		if cards != nil {
			t.Errorf("expected nil on malformed --showbus, got %+v", cards)
		}
	})
}

func TestMergeGhwCards(t *testing.T) {
	t.Run("ghw backfills a card the probe didn't see", func(t *testing.T) {
		probed := []Card{
			{Vendor: VendorNVIDIA, PCIAddress: "0000:01:00.0", Name: "NVIDIA GeForce RTX 4090", MemoryMiB: 24564},
		}
		fetch := func() ([]ghwCard, error) {
			return []ghwCard{
				{Address: "0000:01:00.0", Product: "NVIDIA GeForce RTX 4090", VendorName: "NVIDIA Corporation"},
				{Address: "0000:00:02.0", Product: "UHD Graphics 630", VendorName: "Intel Corporation"},
			}, nil
		}
		out := mergeGhwCards(probed, fetch)
		if len(out) != 2 {
			t.Fatalf("got %d cards, want 2", len(out))
		}
		if out[0].MemoryMiB != 24564 {
			t.Errorf("probed card lost its MemoryMiB after merge: %+v", out[0])
		}
		if out[1].Vendor != VendorIntel || out[1].PCIAddress != "0000:00:02.0" {
			t.Errorf("Intel backfill wrong: %+v", out[1])
		}
	})

	t.Run("ghw fetch error → probed slice returned unchanged", func(t *testing.T) {
		probed := []Card{{Vendor: VendorAMD, PCIAddress: "0000:03:00.0"}}
		out := mergeGhwCards(probed, func() ([]ghwCard, error) {
			return nil, errors.New("ghw exploded")
		})
		if len(out) != 1 || out[0].PCIAddress != "0000:03:00.0" {
			t.Errorf("probed slice not preserved on ghw error: %+v", out)
		}
	})

	t.Run("ghw NVIDIA entry skipped when nvidia-smi already covered the vendor (Windows PCI-format mismatch)", func(t *testing.T) {
		// Repro of the Vultr Windows worker case: nvidia-smi parses
		// "00000000:01:00.0" → "0000:01:00.0" via normalizePCIAddress;
		// ghw on Windows returns a PnP-style address (or empty)
		// that doesn't normalize to the same string. Without the
		// vendor-authoritative skip, the ghw entry slipped through
		// the PCI dedup and inflated gpu_count.
		probed := []Card{
			{Vendor: VendorNVIDIA, PCIAddress: "0000:01:00.0", Name: "NVIDIA A16-8Q", MemoryMiB: 8192},
		}
		fetch := func() ([]ghwCard, error) {
			return []ghwCard{
				{Address: `PCI\VEN_10DE&DEV_25B6&SUBSYS_160210DE&REV_A1\4&12829B10&0&0014`, Product: "NVIDIA A16-8Q", VendorName: "NVIDIA Corporation"},
				{Address: `PCI\VEN_1234&DEV_1111&SUBSYS_11001AF4&REV_02\3&11583659&0&08`, Product: "Microsoft Basic Display Adapter", VendorName: "Microsoft"},
			}, nil
		}
		out := mergeGhwCards(probed, fetch)
		if len(out) != 2 {
			t.Fatalf("got %d cards, want 2 (probed NVIDIA + ghw Microsoft Basic — duplicate ghw NVIDIA should be skipped)", len(out))
		}
		if out[0].Vendor != VendorNVIDIA || out[0].MemoryMiB != 8192 {
			t.Errorf("probed NVIDIA dropped: %+v", out[0])
		}
		if out[1].Vendor != VendorOther || out[1].Name != "Microsoft Basic Display Adapter" {
			t.Errorf("expected ghw Microsoft Basic to backfill, got: %+v", out[1])
		}
	})

	t.Run("ghw duplicate of probed card does not clobber", func(t *testing.T) {
		probed := []Card{
			{Vendor: VendorAMD, PCIAddress: "0000:03:00.0", Name: "AMD Radeon Pro W7900", MemoryMiB: 49152, DriverVersion: "6.1.0"},
		}
		fetch := func() ([]ghwCard, error) {
			return []ghwCard{
				{Address: "0000:03:00.0", Product: "Navi 31", VendorName: "AMD"},
			}, nil
		}
		out := mergeGhwCards(probed, fetch)
		if len(out) != 1 {
			t.Fatalf("got %d, want 1 (dedupe should drop the ghw row)", len(out))
		}
		if out[0].Name != "AMD Radeon Pro W7900" || out[0].MemoryMiB != 49152 {
			t.Errorf("probed row lost its rich data after merge: %+v", out[0])
		}
	})
}

func TestAppleCards(t *testing.T) {
	t.Run("populated Detection.Name → one Card", func(t *testing.T) {
		c, ok := appleCards(Detection{Vendor: VendorApple, State: StateDriverOK, Name: "Apple M3 Pro"})
		if !ok {
			t.Fatal("expected ok=true when Name is populated")
		}
		if c.Vendor != VendorApple || c.Name != "Apple M3 Pro" {
			t.Errorf("Card = %+v", c)
		}
	})

	t.Run("empty Name → no card (defensive guard)", func(t *testing.T) {
		// Future SoC rename breaks appleChipPattern → probe
		// returns StateDriverOK with a blank Name. We must NOT
		// flow a nameless card downstream; the guard returns
		// (Card{}, false) so hardware.pickBestGPUType never
		// sees an empty-vendor-name entry.
		_, ok := appleCards(Detection{Vendor: VendorApple, State: StateDriverOK, Name: ""})
		if ok {
			t.Error("expected ok=false when Name is empty")
		}
	})
}

func TestGhwVendor(t *testing.T) {
	cases := []struct {
		name       string
		product    string
		vendorName string
		want       Vendor
	}{
		{"NVIDIA by product", "NVIDIA GeForce RTX 4090", "", VendorNVIDIA},
		{"AMD by product", "AMD Radeon RX 7900 XTX", "", VendorAMD},
		{"AMD by vendor long form", "Navi 31", "Advanced Micro Devices, Inc.", VendorAMD},
		{"Radeon alias", "Radeon RX 580", "ATI", VendorAMD},
		{"Intel iGPU", "UHD Graphics 630", "Intel Corporation", VendorIntel},
		{"Apple M3", "Apple M3 Pro", "Apple", VendorApple},
		{"unknown → other", "Matrox G200", "Matrox", VendorOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ghwVendor(ghwCard{Product: tc.product, VendorName: tc.vendorName})
			if got != tc.want {
				t.Errorf("ghwVendor = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestInventoryContext drives the whole pipeline through the
// options seam. It's the commit-level acceptance test: single
// invocation, every GPU subprocess stubbed, asserting that the
// returned Inventory has the tri-state per-vendor Detection AND
// the per-card enrichment joined by PCI address — the two things
// the new type is supposed to unify.
func TestInventoryContext(t *testing.T) {
	t.Run("mixed NVIDIA + AMD + Intel node", func(t *testing.T) {
		opts := inventoryOptions{
			probe: stubProbe(map[Vendor]Detection{
				VendorNVIDIA: {Vendor: VendorNVIDIA, State: StateDriverOK, Count: 1},
				VendorAMD:    {Vendor: VendorAMD, State: StateDriverOK, Count: 1},
				VendorApple:  {Vendor: VendorApple, State: StateAbsent},
			}),
			queryNVIDIASMI: stubNVIDIASMI([]nvidiaRow{{
				nvidiaFieldPCIBusID:       "0000:01:00.0",
				nvidiaFieldName:           "NVIDIA A100-SXM4-40GB",
				nvidiaFieldMemoryTotalMiB: "40960",
				nvidiaFieldComputeCap:     "8.0",
				nvidiaFieldDriverVersion:  "550.54.14",
			}}, nil),
			queryRocmSMI: stubRocmSMI(map[string][]byte{
				"--showbus":               []byte("GPU[0]\t\t: 0000:03:00.0\n"),
				"--showdriverversion":     []byte("6.1.0-1234\n"),
				"-i 0 --showmeminfo vram": []byte("GPU[0]\t\t: vram Total Memory (MB): 16384\n"),
			}),
			ghwCards: func() ([]ghwCard, error) {
				return []ghwCard{
					{Address: "0000:01:00.0", Product: "A100", VendorName: "NVIDIA"}, // dup of probed
					{Address: "0000:03:00.0", Product: "Radeon", VendorName: "AMD"},  // dup of probed
					{Address: "0000:00:02.0", Product: "UHD Graphics 630", VendorName: "Intel"},
				}, nil
			},
		}
		inv := inventoryContext(context.Background(), opts.withDefaults())
		// Tri-state map populated for all three vendors.
		if len(inv.Vendors) != 3 {
			t.Fatalf("Vendors map size = %d, want 3", len(inv.Vendors))
		}
		if inv.Vendors[VendorNVIDIA].State != StateDriverOK {
			t.Errorf("NVIDIA state = %v", inv.Vendors[VendorNVIDIA].State)
		}
		if inv.Vendors[VendorApple].State != StateAbsent {
			t.Errorf("Apple state = %v", inv.Vendors[VendorApple].State)
		}
		// Three cards: NVIDIA (from smi), AMD (from rocm), Intel (from ghw backfill).
		// Post-PCI-sort order: 0000:00:02.0 Intel, 0000:01:00.0 NVIDIA, 0000:03:00.0 AMD.
		// PCI-sorted indexing makes "GPU 0" mean the same physical slot across
		// reboots regardless of which probe wrote the row.
		if len(inv.Cards) != 3 {
			t.Fatalf("Cards count = %d, want 3 (got %+v)", len(inv.Cards), inv.Cards)
		}
		if inv.Cards[0].Vendor != VendorIntel || inv.Cards[0].PCIAddress != "0000:00:02.0" {
			t.Errorf("cards[0] (Intel iGPU at 0000:00:02.0): %+v", inv.Cards[0])
		}
		if inv.Cards[1].Vendor != VendorNVIDIA || inv.Cards[1].MemoryMiB != 40960 {
			t.Errorf("cards[1] (NVIDIA at 0000:01:00.0) lost rich nvidia-smi data: %+v", inv.Cards[1])
		}
		if inv.Cards[2].Vendor != VendorAMD || inv.Cards[2].MemoryMiB != 16384 {
			t.Errorf("cards[2] (AMD at 0000:03:00.0): %+v", inv.Cards[2])
		}
	})

	t.Run("CUDA version stamps every NVIDIA card", func(t *testing.T) {
		// Banner-parsed CUDA runtime is node-wide; inventoryContext
		// must stamp the same value onto every NVIDIA Card so the
		// llama.cpp variant selector's cuda_min filter sees a
		// populated value. Two cards, one subprocess call.
		opts := inventoryOptions{
			probe: stubProbe(map[Vendor]Detection{
				VendorNVIDIA: {Vendor: VendorNVIDIA, State: StateDriverOK, Count: 2},
			}),
			queryNVIDIASMI: stubNVIDIASMI([]nvidiaRow{
				{nvidiaFieldPCIBusID: "0000:01:00.0", nvidiaFieldName: "RTX 4090"},
				{nvidiaFieldPCIBusID: "0000:02:00.0", nvidiaFieldName: "RTX 4090"},
			}, nil),
			queryNVIDIACUDAVersion: func(_ context.Context) (string, error) {
				return "12.4", nil
			},
			ghwCards: func() ([]ghwCard, error) { return nil, nil },
		}
		inv := inventoryContext(context.Background(), opts.withDefaults())
		if len(inv.Cards) != 2 {
			t.Fatalf("Cards count = %d, want 2", len(inv.Cards))
		}
		for i, c := range inv.Cards {
			if c.CUDAVersion != "12.4" {
				t.Errorf("cards[%d].CUDAVersion = %q, want %q", i, c.CUDAVersion, "12.4")
			}
		}
	})

	t.Run("missing CUDA banner leaves CUDAVersion empty", func(t *testing.T) {
		opts := inventoryOptions{
			probe: stubProbe(map[Vendor]Detection{
				VendorNVIDIA: {Vendor: VendorNVIDIA, State: StateDriverOK, Count: 1},
			}),
			queryNVIDIASMI: stubNVIDIASMI([]nvidiaRow{
				{nvidiaFieldPCIBusID: "0000:01:00.0", nvidiaFieldName: "RTX 4090"},
			}, nil),
			queryNVIDIACUDAVersion: func(_ context.Context) (string, error) {
				return "", nil // banner parse failed
			},
			ghwCards: func() ([]ghwCard, error) { return nil, nil },
		}
		inv := inventoryContext(context.Background(), opts.withDefaults())
		if len(inv.Cards) != 1 || inv.Cards[0].CUDAVersion != "" {
			t.Errorf("expected empty CUDAVersion, got %+v", inv.Cards)
		}
	})

	t.Run("no GPU on this node", func(t *testing.T) {
		opts := inventoryOptions{
			probe:          stubProbe(nil), // every vendor → StateAbsent
			queryNVIDIASMI: stubNVIDIASMI(nil, nil),
			queryRocmSMI:   stubRocmSMI(nil),
			ghwCards:       func() ([]ghwCard, error) { return nil, nil },
		}
		inv := inventoryContext(context.Background(), opts.withDefaults())
		if len(inv.Cards) != 0 {
			t.Errorf("expected zero cards, got %+v", inv.Cards)
		}
		for v, d := range inv.Vendors {
			if d.State != StateAbsent {
				t.Errorf("vendor %q state = %v, want Absent", v, d.State)
			}
		}
	})
}
