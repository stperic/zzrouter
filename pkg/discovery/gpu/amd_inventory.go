package gpu

import (
	"context"
	"regexp"
	"strconv"
	"strings"
)

// rocmSMIBusLine matches one entry in `rocm-smi --showbus` output:
//
//	GPU[0]		: 0000:03:00.0
//
// The bus capture demands the full BDF shape — 4- or 8-hex
// domain, then bus, device, function — so if a future ROCm fold
// of --showbus and --showid ever lands device-id rows like
// "GPU[0] : 0x73bf" on the same stream, the parser rejects them
// instead of feeding a non-bus token into normalizePCIAddress.
// rocm-smi produces this format consistently across 5.x and 6.x;
// older versions are not in scope.
var rocmSMIBusLine = regexp.MustCompile(`(?i)gpu\[(\d+)\]\s*:\s*([0-9a-f]{4,8}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-9a-f])`)

// rocmSMIVRAMNumber picks the first numeric token after the word
// "vram" in rocm-smi --showmeminfo output. The preceding prose
// varies by version — 5.x emits `vram: 16384 MB`, 6.x emits
// `VRAM Total Memory (B): 17163091968` — but the first digit
// run after "vram" is consistently the value. Unit is inferred
// from magnitude in parseRocmSMIVRAM below.
var rocmSMIVRAMNumber = regexp.MustCompile(`(?i)vram[^\d]*(\d+)`)

// rocmSMIUniqueIDLine matches `--showuniqueid` output:
//
//	GPU[0]		: Unique ID: 0x1234567890abcdef
//
// Capture group 1 is the hex value with optional "0x" prefix.
// Stable across reboots — what zzrouter pins workload affinity to.
var rocmSMIUniqueIDLine = regexp.MustCompile(`(?i)gpu\[\d+\]\s*:\s*unique\s*id\s*:\s*(?:0x)?([0-9a-f]+)`)

// amdCards queries rocm-smi for the per-card detail an AMD
// Inventory needs: the index→PCI map via --showbus, plus per-card
// memory via --showmeminfo vram. The driver version is the same
// for every card on a single node so it's fetched once.
//
// Returns the cards in rocm-smi index order (which matches KFD
// enumeration order, not PCI). Callers that want PCI order
// should sort on Card.PCIAddress afterwards.
//
// Any subprocess error or an empty --showbus response returns
// nil — mergeGhwCards will still populate whatever the AMD
// PCI walker can see from ghw. Partial data is better than
// losing the whole row over a rocm-smi quirk.
func amdCards(ctx context.Context, query func(context.Context, ...string) ([]byte, error)) []Card {
	busOut, err := query(ctx, "--showbus")
	if err != nil || len(busOut) == 0 {
		return nil
	}

	type indexAndBus struct {
		idx int
		bus string
	}
	var entries []indexAndBus
	for _, line := range strings.Split(string(busOut), "\n") {
		m := rocmSMIBusLine.FindStringSubmatch(line)
		if len(m) < 3 {
			continue
		}
		idx, perr := strconv.Atoi(m[1])
		if perr != nil {
			continue
		}
		entries = append(entries, indexAndBus{
			idx: idx,
			bus: normalizePCIAddress(m[2]),
		})
	}
	if len(entries) == 0 {
		return nil
	}

	driverVersion := readRocmSMIDriverVersion(ctx, query)

	cards := make([]Card, 0, len(entries))
	for _, e := range entries {
		idx := e.idx
		c := Card{
			Vendor:        VendorAMD,
			PCIAddress:    e.bus,
			DriverVersion: driverVersion,
			DriverIndex:   &idx, // rocm-smi index — what HIP_VISIBLE_DEVICES selects
		}
		if uid := readRocmSMIUniqueID(ctx, query, e.idx); uid != "" {
			c.UUID = uid
		}
		if mib := readRocmSMIVRAMMiB(ctx, query, e.idx); mib > 0 {
			c.MemoryMiB = mib
		}
		cards = append(cards, c)
	}
	return cards
}

// readRocmSMIUniqueID queries rocm-smi for the per-card "unique id"
// (a 16-hex serial that's stable across reboots and reseats). Empty
// on any error or on driver versions that don't support the field.
// HIP_VISIBLE_DEVICES does NOT consume this directly — runtime pinning
// uses the integer index — but UUID is still the right cross-reboot
// fingerprint for "is this the same physical card I saw last week?"
func readRocmSMIUniqueID(ctx context.Context, query func(context.Context, ...string) ([]byte, error), index int) string {
	out, err := query(ctx, "-i", strconv.Itoa(index), "--showuniqueid")
	if err != nil || len(out) == 0 {
		return ""
	}
	return parseRocmSMIUniqueID(out)
}

// parseRocmSMIUniqueID extracts the 16-hex-character unique id from
// rocm-smi --showuniqueid output. Format across ROCm 5.x/6.x:
//
//	GPU[0]		: Unique ID: 0x1234567890abcdef
//
// Returns the hex string with the "0x" prefix stripped, or empty
// when the line shape doesn't match (driver too old, parse error).
func parseRocmSMIUniqueID(out []byte) string {
	m := rocmSMIUniqueIDLine.FindSubmatch(out)
	if len(m) < 2 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(m[1])))
}

// readRocmSMIVRAMMiB queries rocm-smi for the VRAM size of one
// specific GPU index and returns it in mebibytes. Returns 0 on
// any error or parse failure — the caller treats 0 as "unknown"
// and lets ghw fill in a product-name-only row.
//
// The parser straddles two incompatible rocm-smi output formats:
//
//	ROCm 5.x: "vram: 16384 MB"                        (megabytes)
//	ROCm 6.x: "VRAM Total Memory (B): 17163091968"    (bytes)
//
// Unit detection is by magnitude: anything over 1 GiB is bytes,
// anything smaller is MiB. A "VRAM Used Memory (B): 0" row hits
// the zero-check early and returns 0, which the caller treats as
// "unknown" — safe fallthrough.
func readRocmSMIVRAMMiB(ctx context.Context, query func(context.Context, ...string) ([]byte, error), index int) int64 {
	out, err := query(ctx, "-i", strconv.Itoa(index), "--showmeminfo", "vram")
	if err != nil || len(out) == 0 {
		return 0
	}
	return parseRocmSMIVRAMMiB(out)
}

// parseRocmSMIVRAMMiB is the pure parser split out of
// readRocmSMIVRAMMiB so unit tests can feed it golden bytes
// without a rocm-smi seam.
func parseRocmSMIVRAMMiB(out []byte) int64 {
	m := rocmSMIVRAMNumber.FindSubmatch(out)
	if len(m) < 2 {
		return 0
	}
	v, err := strconv.ParseInt(string(m[1]), 10, 64)
	if err != nil || v <= 0 {
		return 0
	}
	// Unit inference by magnitude. Any value at or above 1 GiB
	// (2^30 bytes) must be a byte count — 1 GiB of VRAM expressed
	// in MiB would be 1024, and no real GPU reports less VRAM
	// than that while using the 5.x MB format. 6.x byte counts
	// dwarf the threshold (16 GiB of VRAM ≈ 1.7e10).
	const oneGiB int64 = 1 << 30
	const bytesPerMiB int64 = 1 << 20
	if v >= oneGiB {
		return v / bytesPerMiB
	}
	return v
}

// readRocmSMIDriverVersion returns the AMD driver version string
// reported by rocm-smi --showdriverversion, trimmed. The same
// value applies to every card managed by the driver, so amdCards
// looks it up once per inventory build.
func readRocmSMIDriverVersion(ctx context.Context, query func(context.Context, ...string) ([]byte, error)) string {
	out, err := query(ctx, "--showdriverversion")
	if err != nil || len(out) == 0 {
		return ""
	}
	return strings.TrimSpace(string(out))
}
