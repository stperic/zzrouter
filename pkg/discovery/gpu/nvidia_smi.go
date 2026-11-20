package gpu

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/stperic/zzrouter/pkg/discovery/resources"
	"github.com/stperic/zzrouter/pkg/host"
)

// Static probes request their own driver fields from the shared CSV parser.
const (
	nvidiaFieldName           = "name"
	nvidiaFieldDriverVersion  = "driver_version"
	nvidiaFieldMemoryTotalMiB = "memory.total"

	nvidiaFieldComputeCap = "compute_cap"
	nvidiaFieldPCIBusID   = "pci.bus_id"
	// nvidiaFieldUUID returns the durable per-card identifier
	// "GPU-abc123-def...". Stable across reboots, card moves, and
	// driver re-enumeration; the right pin for runtime workload
	// affinity. CUDA_VISIBLE_DEVICES accepts UUIDs alongside ints.
	nvidiaFieldUUID = "uuid"
	// nvidiaFieldIndex returns the integer driver index nvidia-smi
	// itself uses (0, 1, …). What CUDA_VISIBLE_DEVICES selects by
	// default and what every nvidia-smi/nvtop output line is keyed
	// on. Less stable than UUID but matches what an operator sees
	// in vendor tools — useful for runtime troubleshooting.
	nvidiaFieldIndex = "index"
)

// nvidiaRow is one parsed nvidia-smi --query-gpu row keyed by the
// requested field names. Lookups for absent fields return "" so
// callers don't need bounds checks. Use the NVIDIAField* constants
// for the common fields.
//
// nvidia-smi reports `[N/A]` for fields that the device or driver
// doesn't support; those values land in the row verbatim and the
// caller is responsible for treating them as "unknown" (an empty
// string also works as a sentinel since strconv.Parse* will error).
type nvidiaRow = map[string]string

// queryNVIDIASMI runs `nvidia-smi --query-gpu=<fields> --format=csv,noheader,nounits`
// and returns one nvidiaRow per GPU. The returned rows are in the
// order nvidia-smi reports — which is **not** PCI bus order on
// multi-GPU boxes (NVML sorts by compute capability descending by
// default, and CUDA_VISIBLE_DEVICES can reorder further). Callers
// that need to associate a row with a specific physical GPU should
// include nvidiaFieldPCIBusID in their query and join by bus ID,
// not by slice index.
//
// Returns (nil, nil) when nvidia-smi is absent from PATH — "no
// driver" is a valid answer for a callsite collecting optional
// metrics, and forcing every caller to branch on exec.ErrNotFound
// would just duplicate the same check. Any other error (permission,
// early exit, parse failure) is returned so callers can log it.
//
// ctx bounds the subprocess; a cancelled ctx aborts nvidia-smi and
// the helper returns ctx.Err() to the caller.
func queryNVIDIASMI(ctx context.Context, fields ...string) ([]nvidiaRow, error) {
	if len(fields) == 0 {
		return nil, nil
	}
	query := "--query-gpu=" + strings.Join(fields, ",")
	out, err := host.CommandContext(ctx, "nvidia-smi", query,
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		// nvidia-smi missing is not an error — it just means no
		// NVIDIA driver stack is installed on this node. Go 1.21+
		// returns ErrNotFound wrapped in *exec.Error and errors.Is
		// unwraps for us, so a single check suffices.
		if errors.Is(err, exec.ErrNotFound) {
			return nil, nil
		}
		// On ExitError, Output() captured nvidia-smi's stderr into
		// err.Stderr — fold it into the returned error so operators
		// can see "Failed to initialize NVML: Driver/library version
		// mismatch" instead of a bare "exit status 255". Pure wrap,
		// so errors.Is(err, context.Canceled) etc. still unwrap.
		return nil, resources.DriverError(err, "nvidia-smi")
	}
	return resources.ParseNVIDIARows(out, fields), nil
}

// populateNVIDIADetection fills in a Detection from one nvidiaRow.
// Fields that are absent from the row, blank, or `[N/A]` are
// silently skipped — Detection's zero values mean "not known".
// Used by the Linux probe to turn a single "best" row into the
// aggregate Detection the install preflight consumes.
func populateNVIDIADetection(d *Detection, row nvidiaRow) {
	if v := row[nvidiaFieldName]; isMeaningful(v) {
		d.Name = v
	}
	if v := row[nvidiaFieldDriverVersion]; isMeaningful(v) {
		d.DriverVersion = v
	}
	if v := row[nvidiaFieldMemoryTotalMiB]; isMeaningful(v) {
		if mb, err := strconv.ParseInt(v, 10, 64); err == nil {
			d.MemoryMB = mb
		}
	}
	if v := row[nvidiaFieldComputeCap]; isMeaningful(v) {
		// "8.9" → "89" to match the dot-free numeric form the
		// rest of zzRouter uses.
		d.ComputeCapability = strings.ReplaceAll(v, ".", "")
	}
}

// nvidiaCUDABannerRe matches the "CUDA Version: X.Y" line nvidia-smi
// prints in its top banner when invoked with no --query-gpu flag.
// The value is the MAX CUDA runtime the installed driver supports,
// not a per-GPU figure — see Card.CUDAVersion docstring.
var nvidiaCUDABannerRe = regexp.MustCompile(`CUDA Version:\s*([0-9]+\.[0-9]+)`)

// queryNVIDIACUDAVersion runs `nvidia-smi` bare and pulls the
// "CUDA Version: X.Y" string out of the banner header. Returns
// ("", nil) when nvidia-smi is absent (matches queryNVIDIASMI's
// "no driver" semantics) or when the banner doesn't contain a
// parseable version — neither case is an error, the caller just
// treats the empty string as "unknown".
//
// The banner changed format across driver releases (the field has
// been present since at least CUDA 11 on Linux and Windows), so
// we match on the literal "CUDA Version:" token rather than
// column positions.
func queryNVIDIACUDAVersion(ctx context.Context) (string, error) {
	out, err := host.CommandContext(ctx, "nvidia-smi").Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", nil
		}
		return "", resources.DriverError(err, "nvidia-smi")
	}
	return parseNVIDIACUDAVersion(out), nil
}

// parseNVIDIACUDAVersion extracts the CUDA runtime version from
// nvidia-smi's banner output. Returns "" when the marker isn't
// present — pure function so tests exercise the parser without
// a subprocess.
func parseNVIDIACUDAVersion(out []byte) string {
	m := nvidiaCUDABannerRe.FindSubmatch(out)
	if len(m) < 2 {
		return ""
	}
	return string(m[1])
}

func isMeaningful(value string) bool { return resources.Meaningful(value) }

func normalizePCIAddress(addr string) string { return resources.NormalizePCIAddress(addr) }
