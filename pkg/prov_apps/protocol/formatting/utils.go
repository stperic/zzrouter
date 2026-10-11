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
