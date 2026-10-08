package gpu

import (
	"context"
	"log/slog"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jaypipes/ghw"

	"github.com/stperic/zzrouter/pkg/utils"
)

// Card is per-device GPU detail — one entry per physical card on
// the node. Populated best-effort: zero values mean "not known",
// not "zero", the same contract as Detection.
//
// Inventory uses Card + Detection as two complementary views of
// the same hardware. Detection answers "is vendor X usable?"
// with tri-state semantics; Card answers "what does card N
// look like?" with a PCI-joined per-device row.
type Card struct {
	Vendor            Vendor `json:"vendor"`
	PCIAddress        string `json:"pci_address,omitempty"`        // canonical "0000:01:00.0"
	Name              string `json:"name,omitempty"`               // product name, e.g. "NVIDIA GeForce RTX 4090"
	MemoryMiB         int64  `json:"memory_mib,omitempty"`         // total VRAM in mebibytes
	DriverVersion     string `json:"driver_version,omitempty"`     // e.g. "550.54.14"
	ComputeCapability string `json:"compute_capability,omitempty"` // numeric form, e.g. "89" for 8.9
	// CUDAVersion is the maximum CUDA runtime version supported by
	// the installed NVIDIA driver, parsed from the nvidia-smi banner
	// ("CUDA Version: 12.4"). Node-wide, not per-card — every NVIDIA
	// Card on the same node carries the same value. Non-NVIDIA cards
	// and nodes without nvidia-smi leave this empty.
	CUDAVersion string `json:"cuda_version,omitempty"`
	// UUID is the durable per-card identifier from the vendor probe:
	// "GPU-abc123-..." from `nvidia-smi --query-gpu=uuid` for NVIDIA,
	// the unique-id field from rocm-smi for AMD. Empty for Apple +
	// Intel + ghw-backfilled rows. Stable across reboots, card swaps,
	// and PCI re-enumeration — the right key for pinning a workload
	// to a specific physical card. Both CUDA_VISIBLE_DEVICES and
	// HIP_VISIBLE_DEVICES accept UUIDs alongside numeric indices.
	UUID string `json:"uuid,omitempty"`
	// DriverIndex is the integer index the vendor's runtime tools
	// report (`nvidia-smi -i N`, `rocm-smi`). What CUDA_VISIBLE_DEVICES
	// / HIP_VISIBLE_DEVICES consume by default and what every
	// nvidia-smi/nvtop output line is keyed on. Less stable than
	// UUID — adding/removing a card or driver re-enumeration can
	// shuffle indices — but matches what the operator sees in vendor
	// tools, which makes it the right value for runtime troubleshooting
	// and the default selector when no UUID is available. Use a
	// pointer so the JSON omits the field for non-vendor-probed cards
	// (Apple, Intel, ghw backfill) where 0 would be ambiguous with
	// "first card".
	DriverIndex *int `json:"driver_index,omitempty"`
}

// Additional vendor tags that never appear on their own in a
// Probe result — InventoryContext uses them only to label ghw-
// sourced Cards for non-compute GPUs (integrated, legacy, unknown
// OEM). ParseVendor accepts these as valid inputs so upstream
// config can filter on them, but passing them to Probe returns
// StateUnknown because there's no supported probe implementation.
const (
	VendorIntel Vendor = "intel"
	VendorOther Vendor = "other"
)

// Inventory is the full GPU picture for a node — the single data
// structure install-time preflight and runtime hardware discovery
// both consume. Producing it runs every GPU subprocess exactly once
// (nvidia-smi, rocm-smi, plus ghw for PCI metadata), so callers
// should avoid re-issuing InventoryContext on a hot path and
// cache where reasonable.
//
// Vendors holds one Detection per vendor that was probed, keyed by
// Vendor. On every platform the set covers NVIDIA + AMD + Apple;
// vendors with no matching hardware land in StateAbsent, which is
// still a meaningful answer for install preflight. Vendors never
// contains VendorIntel or VendorOther — those only appear in Cards.
//
// Cards is the flat list of every physical GPU on the node, one
// entry per card in PCI bus order. For the probed vendors
// (NVIDIA/AMD/Apple) the Card rows are populated from the same
// subprocess call that powered the Detection, so PCIAddress,
// MemoryMiB, DriverVersion, and ComputeCapability carry real data.
// For Intel iGPUs and unknown-vendor cards the row is ghw-sourced
// with Name + PCIAddress only — the other fields stay zero.
type Inventory struct {
	Vendors map[Vendor]Detection `json:"vendors"`
	Cards   []Card               `json:"cards,omitempty"`
}

// inventoryOptions threads the per-subsystem seams InventoryContext
// needs so neither tests nor production reach a package-level
// mutable global. The defaults live on defaultInventoryOptions();
// callers never see this type. Test code calls inventoryContext
// directly with a partial struct.
type inventoryOptions struct {
	queryNVIDIASMI         func(ctx context.Context, fields ...string) ([]nvidiaRow, error)
	queryNVIDIACUDAVersion func(ctx context.Context) (string, error)
	queryRocmSMI           func(ctx context.Context, args ...string) ([]byte, error)
	ghwCards               func() ([]ghwCard, error)
	probe                  func(ctx context.Context, vendor Vendor) (Detection, error)
}

// ghwCard is the minimal shape mergeGhwCards needs from ghw. Kept
// local so tests can stub ghw without importing the full library
// into their fixture plumbing.
type ghwCard struct {
	Address    string
	Product    string
	VendorName string
}

func (o inventoryOptions) withDefaults() inventoryOptions {
	if o.queryNVIDIASMI == nil {
		o.queryNVIDIASMI = queryNVIDIASMI
	}
	if o.queryNVIDIACUDAVersion == nil {
		o.queryNVIDIACUDAVersion = queryNVIDIACUDAVersion
	}
	if o.queryRocmSMI == nil {
		o.queryRocmSMI = queryRocmSMI
	}
	if o.ghwCards == nil {
		o.ghwCards = defaultGhwCards
	}
	if o.probe == nil {
		o.probe = ProbeContext
	}
	return o
}

// defaultGhwCards is the production ghw adapter. Kept as a package-
// level function so inventoryOptions.ghwCards defaults to it and
// the test-seam path never touches ghw at all.
func defaultGhwCards() ([]ghwCard, error) {
	if runtime.GOOS == "darwin" {
		return nil, nil
	}
	info, err := ghw.GPU()
	if err != nil || info == nil {
		return nil, err
	}
	out := make([]ghwCard, 0, len(info.GraphicsCards))
	for _, gc := range info.GraphicsCards {
		card := ghwCard{Address: gc.Address}
		if gc.DeviceInfo != nil {
			card.Product = gc.DeviceInfo.Product.Name
			card.VendorName = gc.DeviceInfo.Vendor.Name
		}
		out = append(out, card)
	}
	return out, nil
}

// InventoryContext collects the full GPU picture in one pass. It
// runs ProbeContext once per probed vendor (NVIDIA, AMD, Apple),
// then fans out per-card enrichment based on the resulting states:
//
//   - NVIDIA + StateDriverOK → query nvidia-smi once for the full
//     field set and build one Card per returned row. PCI bus ID
//     comes from the query, memory/compute-cap/driver-version come
//     from the same row.
//
//   - AMD + StateDriverOK → query rocm-smi --showbus for the
//     index→PCI map, then --showmeminfo + --showdriverversion for
//     each indexed card. Legacy fallback (index-only) kicks in if
//     --showbus is missing.
//
//   - Apple + StateDriverOK → produce a single Card with Name from
//     the chip model. Memory/driver-version/compute-cap stay zero
//     because Apple Silicon's unified memory isn't a per-GPU figure.
//
// Finally ghw runs (where available) so integrated/unknown cards
// that the probed-vendor path missed still appear in the Cards
// slice — deduped by canonical PCI address so a ghw hit for a
// card the probe already populated doesn't clobber the richer row.
//
// InventoryContext never returns an error. A healthy "no GPU
// on this node" result is Inventory{Vendors: {…all StateAbsent
// …}, Cards: nil}; catastrophic subprocess failures (ghw
// library crash, nvidia-smi kernel-module mismatch) land in
// Detection.Diagnostic via ProbeContext for callers that want
// to log them. Callers must branch on state / card count, not
// on an error return — same contract LiveMetricsContext uses.
func InventoryContext(ctx context.Context) Inventory {
	// Honor a ctx-scoped inventory cache — preflight-then-install
	// flows wrap their ctx with WithCachedInventory so both the
	// preflight probe and the variant-selection discovery share a
	// single nvidia-smi / rocm-smi / ghw pass.
	if inv, ok := cachedInventory(ctx); ok {
		return *inv
	}
	return inventoryContext(ctx, inventoryOptions{}.withDefaults())
}

// probeTimeout bounds one detection pass. nvidia-smi on a broken driver
// can hang, and this runs on the startup path.
const probeTimeout = 10 * time.Second

// negativeRetryInterval is the delay before the FIRST re-test of a
// "no usable GPU" result. Each attempt costs an nvidia-smi / rocm-smi /
// ghw subprocess pass.
const negativeRetryInterval = time.Minute

// negativeRetryMax caps the backoff. A node that genuinely has no
// accelerator is negative forever, so a fixed interval would buy nothing
// after the first few minutes and keep paying a subprocess pass a minute
// for the life of the process. Backing off to this ceiling still notices
// a GPU that appears on day 30, at roughly 48 probes a day instead of
// 1440.
const negativeRetryMax = 30 * time.Minute

var (
	probeOnce sync.Once
	probeMu   sync.RWMutex
	probeInv  Inventory
	probedAt  time.Time
	// negativeStreak counts consecutive probes that found no usable GPU.
	// Guarded by probeMu with probeInv/probedAt: it is part of the same
	// answer and must not be read against a different probe's result.
	negativeStreak int
	reprobing      atomic.Bool
)

var ready = make(chan struct{})

// load detects GPU hardware and primes the cache.
func load() {
	probeOnce.Do(func() {
		storeInventory(probeNow())
		close(ready)
	})
}

// probeNow runs one detection pass. No caller ctx: this feeds a
// process-level cache.
func probeNow() Inventory {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	return InventoryContext(ctx)
}

func storeInventory(inv Inventory) {
	probeMu.Lock()
	defer probeMu.Unlock()
	probeInv = inv
	probedAt = utils.Now()
	if inv.hasUsableGPU() {
		negativeStreak = 0
		return
	}
	negativeStreak++
}

func cachedInv() (Inventory, time.Time, int) {
	probeMu.RLock()
	defer probeMu.RUnlock()
	return probeInv, probedAt, negativeStreak
}

// negativeBackoff is the delay owed after streak consecutive misses:
// negativeRetryInterval doubling per miss, capped at negativeRetryMax.
func negativeBackoff(streak int) time.Duration {
	d := negativeRetryInterval
	for i := 1; i < streak; i++ {
		if d >= negativeRetryMax {
			break
		}
		d *= 2
	}
	if d > negativeRetryMax {
		return negativeRetryMax
	}
	return d
}

// hasUsableGPU reports whether any vendor's driver stack came up.
func (i Inventory) hasUsableGPU() bool {
	for _, d := range i.Vendors {
		if d.State == StateDriverOK {
			return true
		}
	}
	return false
}

// staleNegative reports that the cached answer is "no usable GPU" and is
// old enough to be worth re-testing.
//
// A positive result is cached for the process lifetime: the card list is
// static hardware. A negative one is not trustworthy in the same way,
// because zzRouter routinely starts before the GPU driver does — a node
// whose /dev/nvidia* appeared after boot would otherwise advertise no
// accelerator until someone restarted it, and nothing would ever schedule
// a model onto the largest machine in the cluster.
// The retry backs off (see negativeBackoff) so a node that truly has no
// accelerator stops paying for the answer, without ever giving up on one
// that gains a driver later.
func staleNegative(inv Inventory, at time.Time, streak int) bool {
	return !inv.hasUsableGPU() && utils.Now().Sub(at) >= negativeBackoff(streak)
}

// reprobeAsync re-runs detection in the background, at most one at a
// time. Callers on the non-blocking path get the current answer now and
// the corrected one on a later call.
func reprobeAsync() {
	if !reprobing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer reprobing.Store(false)
		storeInventory(probeNow())
	}()
}

// reprobeSync re-runs detection for a caller that asked to block, and
// returns the fresh answer.
//
// Single-flighted against reprobeAsync through the same flag: a probe
// costs an nvidia-smi / rocm-smi / ghw pass bounded by probeTimeout, and
// this path is wired per-request on the discovery endpoint. Without it,
// N concurrent requests to a GPU-less node would run N probes, and a
// sync probe racing a background one could store the older result last.
// A caller that arrives while a probe is already running takes the
// current cached answer rather than queueing behind a second one.
func reprobeSync() Inventory {
	if !reprobing.CompareAndSwap(false, true) {
		inv, _, _ := cachedInv()
		return inv
	}
	defer reprobing.Store(false)
	inv := probeNow()
	storeInventory(inv)
	return inv
}

// StartAsync begins GPU detection in the background. Non-blocking —
// results are available via List once the probe completes.
func StartAsync() { go load() }

// List returns the GPU inventory.
//
// refresh=true: triggers detection if needed and blocks until complete.
// refresh=false: returns the cached result if the probe has finished,
// or an empty Inventory if it's still running or was never started.
// Safe to call concurrently with an in-progress probe.
//
// Either way, a cached "no GPU" older than negativeRetryInterval is
// re-tested — synchronously when the caller asked to refresh, in the
// background otherwise. See staleNegative.
func List(refresh bool) Inventory {
	if refresh {
		load() // sync.Once serializes; blocks until probe completes
		inv, at, streak := cachedInv()
		if staleNegative(inv, at, streak) {
			inv = reprobeSync()
		}
		return inv
	}
	// Non-blocking: return cached result only if probe is done.
	select {
	case <-ready:
		inv, at, streak := cachedInv()
		if staleNegative(inv, at, streak) {
			reprobeAsync()
		}
		return inv
	default:
		return Inventory{}
	}
}

// inventoryContext is the test-injection seam. Production callers
// go through InventoryContext which supplies the defaults; unit
// tests pass a partial inventoryOptions with stubbed query
// functions so they can pin any node state deterministically.
func inventoryContext(ctx context.Context, opts inventoryOptions) Inventory {
	inv := Inventory{
		Vendors: make(map[Vendor]Detection, 3),
	}

	// Step 1 — tri-state probe per vendor. Every platform answers
	// all three; an unsupported vendor (e.g. Apple on Linux) just
	// returns StateAbsent cheaply.
	for _, v := range []Vendor{VendorNVIDIA, VendorAMD, VendorApple} {
		det, _ := opts.probe(ctx, v)
		inv.Vendors[v] = det
	}

	// Step 2 — per-card enrichment for each vendor the probe says
	// has a working driver. On a no-GPU box this is a no-op.
	if inv.Vendors[VendorNVIDIA].State == StateDriverOK {
		nCards := nvidiaCards(ctx, opts.queryNVIDIASMI)
		// Banner-parsed CUDA runtime is node-wide — one nvidia-smi
		// invocation, then stamp every NVIDIA card with the same
		// value so downstream variant-selection filters (llama.cpp's
		// cuda_min) see a populated field.
		if len(nCards) > 0 {
			// Surface non-nil errors — a non-zero exit from nvidia-smi
			// (NVML init failure, GPU fell off PCIe, driver/library
			// mismatch) leaves CUDAVersion empty and causes the
			// llama.cpp variant selector to silently pick the CPU
			// build. Debug-level so the happy path stays quiet; an
			// operator debugging "why did I get the CPU variant?" can
			// raise the log level and see the root cause.
			cuda, err := opts.queryNVIDIACUDAVersion(ctx)
			if err != nil {
				slog.Debug("nvidia-smi CUDA banner probe failed",
					"err", err)
			}
			if cuda != "" {
				for i := range nCards {
					nCards[i].CUDAVersion = cuda
				}
			}
		}
		inv.Cards = append(inv.Cards, nCards...)
	}
	if inv.Vendors[VendorAMD].State == StateDriverOK {
		inv.Cards = append(inv.Cards, amdCards(ctx, opts.queryRocmSMI)...)
	}
	if inv.Vendors[VendorApple].State == StateDriverOK {
		if c, ok := appleCards(inv.Vendors[VendorApple]); ok {
			inv.Cards = append(inv.Cards, c)
		}
	}

	// Step 3 — ghw backfill so Intel iGPUs and unknown OEMs appear
	// in Cards even though no tri-state probe claims them. Dedupes
	// against the richer probe-sourced rows by canonical PCI address.
	inv.Cards = mergeGhwCards(inv.Cards, opts.ghwCards)

	// Step 4 — sort by PCI address so the cluster-TUI's "GPU 0",
	// "GPU 1" labels mean the same physical slot across reboots.
	// nvidia-smi orders by NVML compute-cap-descending by default,
	// rocm-smi by driver enumeration; ghw by lspci. Without an
	// explicit sort, a heterogeneous box would re-shuffle indices
	// on every probe. Cards with no PCIAddress (Apple unified-mem
	// row, ghw entries that lost their address) sort to the end.
	sort.SliceStable(inv.Cards, func(i, j int) bool {
		ai, aj := inv.Cards[i].PCIAddress, inv.Cards[j].PCIAddress
		if ai == "" && aj == "" {
			return false
		}
		if ai == "" {
			return false
		}
		if aj == "" {
			return true
		}
		return ai < aj
	})

	return inv
}

// nvidiaCards issues one nvidia-smi query covering every field an
// NVIDIA Card needs and returns the rows as Cards. Failures are
// swallowed — the outer Inventory build treats zero cards as a
// best-effort result consistent with the existing per-vendor
// Detection.
func nvidiaCards(ctx context.Context, query func(context.Context, ...string) ([]nvidiaRow, error)) []Card {
	rows, err := query(ctx,
		nvidiaFieldPCIBusID,
		nvidiaFieldName,
		nvidiaFieldMemoryTotalMiB,
		nvidiaFieldComputeCap,
		nvidiaFieldDriverVersion,
		nvidiaFieldUUID,
		nvidiaFieldIndex,
	)
	if err != nil || len(rows) == 0 {
		return nil
	}
	cards := make([]Card, 0, len(rows))
	for _, row := range rows {
		c := Card{Vendor: VendorNVIDIA}
		if v := row[nvidiaFieldPCIBusID]; isMeaningful(v) {
			c.PCIAddress = normalizePCIAddress(v)
		}
		if v := row[nvidiaFieldName]; isMeaningful(v) {
			c.Name = v
		}
		if v := row[nvidiaFieldMemoryTotalMiB]; isMeaningful(v) {
			if mib, perr := strconv.ParseInt(v, 10, 64); perr == nil {
				c.MemoryMiB = mib
			}
		}
		if v := row[nvidiaFieldComputeCap]; isMeaningful(v) {
			// "8.9" → "89" to match the Detection convention.
			c.ComputeCapability = stripDots(v)
		}
		if v := row[nvidiaFieldDriverVersion]; isMeaningful(v) {
			c.DriverVersion = v
		}
		if v := row[nvidiaFieldUUID]; isMeaningful(v) {
			c.UUID = v
		}
		if v := row[nvidiaFieldIndex]; isMeaningful(v) {
			if idx, perr := strconv.Atoi(v); perr == nil {
				c.DriverIndex = &idx
			}
		}
		cards = append(cards, c)
	}
	return cards
}

// appleCards lifts a single Card out of the Apple Detection.
// The chip model landed in Detection.Name during
// probeAppleSilicon; memory and driver fields are intentionally
// left zero because Apple Silicon's unified memory is not a
// per-GPU value.
//
// Returns (Card{}, false) when the probe succeeded but failed
// to extract a chip name — better to omit the card entirely
// than to let a nameless row flow downstream and confuse
// hardware.pickBestGPUType or the variant selector. This can
// only happen if sysctl returns the brand string but the
// appleChipPattern regex fails to match, which shouldn't occur
// on any shipping macOS build — the guard is defensive against
// a future SoC rename.
func appleCards(d Detection) (Card, bool) {
	if d.Name == "" {
		return Card{}, false
	}
	return Card{
		Vendor: VendorApple,
		Name:   d.Name,
	}, true
}

// mergeGhwCards invokes the supplied ghw fetcher and appends any
// card whose canonical PCI address is not already represented in
// probed. Probed cards always win: their rows carry real memory/
// driver/compute-cap data, ghw only knows product name + PCI. On
// Darwin the default fetcher short-circuits to (nil, nil) so this
// is effectively a no-op. Any ghw error is swallowed — a partial
// Cards slice is better than losing the probed rows over an
// ancillary failure.
func mergeGhwCards(probed []Card, fetch func() ([]ghwCard, error)) []Card {
	cards, err := fetch()
	if err != nil || len(cards) == 0 {
		return probed
	}
	// Track PCI addresses already covered by probe-sourced cards
	// so a successful PCI-format match still dedupes.
	seen := make(map[string]struct{}, len(probed))
	// Track which vendors are already authoritatively covered by a
	// probe (nvidia-smi → NVIDIA, rocm-smi → AMD, appleCards → Apple).
	// ghw is intentional backfill for Intel iGPUs + unknown OEMs;
	// when the vendor's authoritative probe already produced cards,
	// any ghw entry for the same vendor is a duplicate from a
	// PCI-format mismatch (Windows ghw returns PnP-style
	// addresses while nvidia-smi returns 0000:bb:dd.f form, so the
	// PCI dedup misses) and would inflate gpu_count.
	probedVendors := make(map[Vendor]struct{}, 4)
	for _, c := range probed {
		if c.PCIAddress != "" {
			seen[c.PCIAddress] = struct{}{}
		}
		probedVendors[c.Vendor] = struct{}{}
	}
	out := probed
	for _, gc := range cards {
		v := ghwVendor(gc)
		// Probe-authoritative vendor already covered → skip the ghw
		// row outright. VendorIntel + VendorOther are NOT in this
		// map because no probe owns them, so they fall through
		// to the PCI-dedup path below as before.
		if v == VendorNVIDIA || v == VendorAMD || v == VendorApple {
			if _, covered := probedVendors[v]; covered {
				continue
			}
		}
		addr := normalizePCIAddress(gc.Address)
		if addr != "" {
			if _, dup := seen[addr]; dup {
				continue
			}
			seen[addr] = struct{}{}
		}
		out = append(out, Card{
			Vendor:     v,
			PCIAddress: addr,
			Name:       gc.Product,
		})
	}
	return out
}

// lower is a shorthand for strings.ToLower — keeps ghwVendor
// readable without pulling the long package name into every
// branch.
func lower(s string) string { return strings.ToLower(s) }

// containsAny reports whether s contains any of the provided
// substrings. Used for fuzzy vendor matching on ghw's free-form
// product / vendor name fields.
func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// stripDots removes every "." from s. Used to rewrite nvidia-smi's
// compute_cap ("8.9") into the dot-free numeric form the rest of
// zzRouter uses ("89").
func stripDots(s string) string { return strings.ReplaceAll(s, ".", "") }

// ghwVendor maps a ghw card entry to our Vendor enum by looking
// at both the product name and the vendor name fields — ghw
// doesn't always populate one or the other. Anything that doesn't
// match NVIDIA/AMD/Apple/Intel drops to VendorOther.
func ghwVendor(gc ghwCard) Vendor {
	product := lower(gc.Product)
	vendor := lower(gc.VendorName)
	switch {
	case containsAny(product, "nvidia") || containsAny(vendor, "nvidia"):
		return VendorNVIDIA
	case containsAny(product, "amd", "radeon") || containsAny(vendor, "amd", "advanced micro devices"):
		return VendorAMD
	case containsAny(product, "apple") || containsAny(vendor, "apple"):
		return VendorApple
	case containsAny(product, "intel") || containsAny(vendor, "intel"):
		return VendorIntel
	default:
		return VendorOther
	}
}
