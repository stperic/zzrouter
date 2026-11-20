package resources

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
)

const maxDevices = 64

// NVIDIAPools reads dedicated pools and driver attribution in one pair of queries.
func NVIDIAPools(ctx context.Context) ([]Pool, error) {
	return nvidiaPools(ctx, boundedNVIDIAOutput)
}

func nvidiaPools(ctx context.Context, query func(context.Context, ...string) ([]byte, error)) ([]Pool, error) {
	fields := []string{"uuid", "index", "memory.total", "memory.free", "memory.used", "pci.bus_id", "utilization.gpu", "power.draw"}
	output, err := query(ctx, "--query-gpu="+strings.Join(fields, ","), "--format=csv,noheader,nounits")
	if err != nil {
		return nil, err
	}
	rows := ParseNVIDIARows(output, fields)
	if len(rows) == 0 || len(rows) > maxDevices {
		return nil, fmt.Errorf("GPU memory observation unavailable")
	}
	var pools []Pool
	var incomplete bool
	for _, row := range rows {
		pool := Pool{
			Kind: GPU, Vendor: "nvidia", TotalMiB: quantity(row["memory.total"]),
			UsedMiB: quantity(row["memory.used"]), FreeMiB: quantity(row["memory.free"]),
			UtilizationPct: reading(row["utilization.gpu"]), PowerW: reading(row["power.draw"]),
			Holders: []Holder{},
		}
		if Meaningful(row["uuid"]) {
			pool.ID = row["uuid"]
		}
		if Meaningful(row["pci.bus_id"]) {
			pool.PCIAddress = NormalizePCIAddress(row["pci.bus_id"])
		}
		if index, err := strconv.Atoi(row["index"]); err == nil && index >= 0 {
			pool.Index = &index
		}
		if pool.TotalMiB != nil && *pool.TotalMiB == 0 {
			pool.TotalMiB = nil
		}
		if pool.TotalMiB != nil {
			if pool.UsedMiB != nil && *pool.UsedMiB > *pool.TotalMiB {
				pool.UsedMiB = nil
			}
			if pool.FreeMiB != nil && *pool.FreeMiB > *pool.TotalMiB {
				pool.FreeMiB = nil
			}
		}
		_, valid := pool.AsMemoryDevice()
		incomplete = incomplete || !valid
		if len(row) != len(fields) || pool.ID == "" && pool.PCIAddress == "" ||
			pool.TotalMiB == nil && pool.FreeMiB == nil && pool.UsedMiB == nil {
			incomplete = true
			continue
		}
		pools = append(pools, pool)
	}
	if incomplete {
		err = fmt.Errorf("GPU memory observation incomplete; no safe admission budget")
	}
	if len(pools) == 0 {
		return nil, err
	}
	if processErr := nvidiaHolders(ctx, query, pools); processErr != nil {
		return pools, processErr
	}
	return pools, err
}

func nvidiaHolders(ctx context.Context, query func(context.Context, ...string) ([]byte, error), pools []Pool) error {
	fields := []string{"gpu_uuid", "pid", "used_gpu_memory"}
	output, err := query(ctx, "--query-compute-apps="+strings.Join(fields, ","), "--format=csv,noheader,nounits")
	if err != nil || ctx.Err() != nil {
		for i := range pools {
			pools[i].Holders = nil
			pools[i].ProcessReason = "driver process attribution unavailable"
		}
		return ctx.Err()
	}
	for _, row := range ParseNVIDIARows(output, fields) {
		pid, err := strconv.Atoi(row["pid"])
		if err != nil || pid <= 0 {
			continue
		}
		for i := range pools {
			if pools[i].ID != "" && pools[i].ID == row["gpu_uuid"] {
				pools[i].Holders = append(pools[i].Holders, Holder{PID: pid, MemoryMiB: quantity(row["used_gpu_memory"])})
			}
		}
	}
	return nil
}

func quantity(value string) *int64 {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return nil
	}
	return &n
}

func reading(value string) *float64 {
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return nil
	}
	return &n
}

type boundedOutput struct {
	data      []byte
	truncated bool
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	n := len(data)
	remaining := (64 << 10) - len(b.data)
	if len(data) > remaining {
		data = data[:remaining]
		b.truncated = true
	}
	b.data = append(b.data, data...)
	return n, nil
}

func boundedNVIDIAOutput(ctx context.Context, args ...string) ([]byte, error) {
	command := host.CommandContext(ctx, "nvidia-smi", args...)
	var stdout, stderr boundedOutput
	command.Stdout, command.Stderr = &stdout, &stderr
	command.WaitDelay = time.Second
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("GPU driver query: %w", err)
	}
	if stdout.truncated {
		return nil, fmt.Errorf("GPU driver response exceeds observation limit")
	}
	return stdout.data, nil
}

// ParseNVIDIARows retains only the fields supplied by each driver row.
func ParseNVIDIARows(out []byte, fields []string) []map[string]string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	rows := make([]map[string]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		row := make(map[string]string, len(fields))
		for i, name := range fields {
			if i < len(parts) {
				row[name] = strings.TrimSpace(parts[i])
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// Meaningful reports whether a driver field carries an observation.
func Meaningful(v string) bool {
	if v == "" {
		return false
	}
	if strings.EqualFold(v, "[N/A]") || strings.EqualFold(v, "N/A") {
		return false
	}
	return true
}

// NormalizePCIAddress aligns NVML slot identifiers with static inventory.
func NormalizePCIAddress(addr string) string {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if addr == "" {
		return ""
	}
	parts := strings.SplitN(addr, ":", 2)
	if len(parts) != 2 {
		return addr
	}
	domain := strings.TrimLeft(parts[0], "0")
	if domain == "" {
		domain = "0"
	}
	// Pad to 4 hex digits to match ghw's convention.
	for len(domain) < 4 {
		domain = "0" + domain
	}
	return domain + ":" + parts[1]
}
