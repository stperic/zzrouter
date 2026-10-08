package hardware

import (
	"context"
	"fmt"

	"github.com/shirou/gopsutil/v4/disk"
)

// DiscoverDisk discovers disk space information for all mounted filesystems
func DiscoverDisk(ctx context.Context) ([]DiskInfo, []error) {
	var disks []DiskInfo
	var errors []error

	// Get partition information
	partitions, err := disk.Partitions(true)
	if err != nil {
		errors = append(errors, fmt.Errorf("failed to get disk partitions: %w", err))
		return disks, errors
	}

	// Get usage information for each partition
	for _, partition := range partitions {
		// Skip certain filesystem types that are not useful for storage
		if shouldSkipFilesystem(partition.Fstype) {
			continue
		}

		usage, err := disk.Usage(partition.Mountpoint)
		if err != nil {
			errors = append(errors, fmt.Errorf("failed to get usage for %s: %w", partition.Mountpoint, err))
			continue
		}

		diskInfo := DiskInfo{
			Path:        partition.Mountpoint,
			Device:      partition.Device,
			FSType:      partition.Fstype,
			TotalGB:     float64(usage.Total) / BytesPerGB, // Convert to GB
			UsedGB:      float64(usage.Used) / BytesPerGB,
			AvailableGB: float64(usage.Free) / BytesPerGB,
			UsedPercent: usage.UsedPercent,
		}

		disks = append(disks, diskInfo)
	}

	return disks, errors
}

// shouldSkipFilesystem determines if a filesystem type should be skipped
func shouldSkipFilesystem(fstype string) bool {
	// Skip virtual filesystems and special filesystems
	skipTypes := map[string]struct{}{
		"tmpfs":           {}, // Temporary filesystem
		"devtmpfs":        {}, // Device temporary filesystem
		"proc":            {}, // Process filesystem
		"sysfs":           {}, // System filesystem
		"devpts":          {}, // Pseudo-terminal filesystem
		"securityfs":      {}, // Security filesystem
		"cgroup":          {}, // Control group filesystem
		"cgroup2":         {}, // Control group v2 filesystem
		"pstore":          {}, // Persistent storage filesystem
		"autofs":          {}, // Automount filesystem
		"mqueue":          {}, // Message queue filesystem
		"hugetlbfs":       {}, // Huge page filesystem
		"debugfs":         {}, // Debug filesystem
		"tracefs":         {}, // Trace filesystem
		"fuse.gvfsd-fuse": {}, // GVFS fuse
		"fuse":            {}, // Generic FUSE (skip most)
		"overlay":         {}, // Docker overlay filesystem
	}

	_, found := skipTypes[fstype]
	return found
}
