package formatting

import (
	"fmt"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// Size constants for formatting
const (
	KB = 1024
	MB = KB * 1024
	GB = MB * 1024
	TB = GB * 1024
)

// FormatSize formats size in bytes to human-readable format (binary, 1024-based).
// Returns "-" for zero or negative values.
func FormatSize(sizeBytes int64) string {
	if sizeBytes <= 0 {
		return "-"
	}

	switch {
	case sizeBytes >= TB:
		return fmt.Sprintf("%.1f TB", float64(sizeBytes)/float64(TB))
	case sizeBytes >= GB:
		return fmt.Sprintf("%.1f GB", float64(sizeBytes)/float64(GB))
	case sizeBytes >= MB:
		return fmt.Sprintf("%.1f MB", float64(sizeBytes)/float64(MB))
	case sizeBytes >= KB:
		return fmt.Sprintf("%.1f KB", float64(sizeBytes)/float64(KB))
	default:
		return fmt.Sprintf("%d B", sizeBytes)
	}
}

// FormatSpeed formats speed in bytes per second to human-readable format.
// Returns "-" for zero or negative values.
func FormatSpeed(bytesPerSec int64) string {
	if bytesPerSec <= 0 {
		return "-"
	}

	switch {
	case bytesPerSec >= GB:
		return fmt.Sprintf("%.1f GB/s", float64(bytesPerSec)/float64(GB))
	case bytesPerSec >= MB:
		return fmt.Sprintf("%.1f MB/s", float64(bytesPerSec)/float64(MB))
	case bytesPerSec >= KB:
		return fmt.Sprintf("%.1f KB/s", float64(bytesPerSec)/float64(KB))
	default:
		return fmt.Sprintf("%d B/s", bytesPerSec)
	}
}

// FormatDuration formats a duration to human-readable format.
// Examples: "5s", "3m", "2h 15m", "1d 5h"
func FormatDuration(d time.Duration) string {
	if d < 0 {
		return "-"
	}

	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		hours := int(d.Hours())
		mins := int(d.Minutes()) % 60
		if mins == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh %dm", hours, mins)
	default:
		days := int(d.Hours()) / 24
		hours := int(d.Hours()) % 24
		if hours == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd %dh", days, hours)
	}
}

// FormatUptime calculates and formats uptime from a start time.
// Returns "-" if startTime is zero.
func FormatUptime(startTime time.Time) string {
	if startTime.IsZero() {
		return "-"
	}
	return FormatDuration(time.Since(startTime))
}

// FormatUptimeRFC3339 parses an RFC3339 timestamp and formats the uptime.
// Returns "-" if the timestamp is empty or invalid.
func FormatUptimeRFC3339(timestamp string) string {
	if timestamp == "" {
		return "-"
	}

	t, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return "-"
	}

	return FormatUptime(t)
}

// FormatProgress formats progress as a percentage.
// If total is 0, returns "-".
func FormatProgress(current, total int64) string {
	if total <= 0 {
		return "-"
	}

	percent := float64(current) / float64(total) * 100
	return fmt.Sprintf("%.1f%%", percent)
}

// FormatModifiedTime formats a "2006-01-02 15:04" timestamp as relative time
// if recent (e.g., "just now", "3 hours ago", "yesterday"), otherwise returns
// the original string. Returns "N/A" for empty input.
func FormatModifiedTime(modified string) string {
	if modified == "" {
		return "N/A"
	}
	// Parse and reformat if needed
	t, err := time.Parse("2006-01-02 15:04", modified)
	if err != nil {
		return modified
	}

	// Show relative time if recent
	now := utils.Now()
	duration := now.Sub(t)

	if duration < 24*time.Hour {
		hours := int(duration.Hours())
		switch hours {
		case 0:
			return "just now"
		case 1:
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	} else if duration < 7*24*time.Hour {
		days := int(duration.Hours() / 24)
		if days == 1 {
			return "yesterday"
		}
		return fmt.Sprintf("%d days ago", days)
	}

	return modified
}

// TruncateID truncates an ID to a short display format (like Docker).
func TruncateID(id string, length int) string {
	if length <= 0 {
		length = 12
	}
	if len(id) <= length {
		return id
	}
	return id[:length]
}
