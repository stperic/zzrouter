package formatting

import (
	"testing"
	"time"
)

func TestFormatSize(t *testing.T) {
	tests := []struct {
		name     string
		size     int64
		expected string
	}{
		{"zero", 0, "-"},
		{"negative", -100, "-"},
		{"bytes", 500, "500 B"},
		{"kilobytes", 1536, "1.5 KB"},
		{"megabytes", 1572864, "1.5 MB"},
		{"gigabytes", 1610612736, "1.5 GB"},
		{"terabytes", 1649267441664, "1.5 TB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatSize(tt.size)
			if result != tt.expected {
				t.Errorf("FormatSize(%d) = %q, want %q", tt.size, result, tt.expected)
			}
		})
	}
}

func TestFormatSpeed(t *testing.T) {
	tests := []struct {
		name     string
		speed    int64
		expected string
	}{
		{"zero", 0, "-"},
		{"negative", -100, "-"},
		{"bytes per sec", 500, "500 B/s"},
		{"kilobytes per sec", 1536, "1.5 KB/s"},
		{"megabytes per sec", 1572864, "1.5 MB/s"},
		{"gigabytes per sec", 1610612736, "1.5 GB/s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatSpeed(tt.speed)
			if result != tt.expected {
				t.Errorf("FormatSpeed(%d) = %q, want %q", tt.speed, result, tt.expected)
			}
		})
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		expected string
	}{
		{"negative", -time.Second, "-"},
		{"seconds", 45 * time.Second, "45s"},
		{"minutes", 5 * time.Minute, "5m"},
		{"hours and minutes", 2*time.Hour + 30*time.Minute, "2h 30m"},
		{"hours only", 3 * time.Hour, "3h"},
		{"days and hours", 26 * time.Hour, "1d 2h"},
		{"days only", 48 * time.Hour, "2d"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatDuration(tt.duration)
			if result != tt.expected {
				t.Errorf("FormatDuration(%v) = %q, want %q", tt.duration, result, tt.expected)
			}
		})
	}
}

func TestFormatProgress(t *testing.T) {
	tests := []struct {
		name     string
		current  int64
		total    int64
		expected string
	}{
		{"zero total", 50, 0, "-"},
		{"negative total", 50, -100, "-"},
		{"half", 50, 100, "50.0%"},
		{"complete", 100, 100, "100.0%"},
		{"partial", 33, 100, "33.0%"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatProgress(tt.current, tt.total)
			if result != tt.expected {
				t.Errorf("FormatProgress(%d, %d) = %q, want %q", tt.current, tt.total, result, tt.expected)
			}
		})
	}
}

func TestTruncateID(t *testing.T) {
	tests := []struct {
		name     string
		id       string
		length   int
		expected string
	}{
		{"short id", "abc123", 12, "abc123"},
		{"exact length", "abc123def456", 12, "abc123def456"},
		{"truncate", "abc123def456ghi789", 12, "abc123def456"},
		{"default length", "abc123def456ghi789", 0, "abc123def456"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := TruncateID(tt.id, tt.length)
			if result != tt.expected {
				t.Errorf("TruncateID(%q, %d) = %q, want %q", tt.id, tt.length, result, tt.expected)
			}
		})
	}
}

func TestFormatUptimeRFC3339(t *testing.T) {
	// Test empty timestamp
	result := FormatUptimeRFC3339("")
	if result != "-" {
		t.Errorf("FormatUptimeRFC3339(\"\") = %q, want \"-\"", result)
	}

	// Test invalid timestamp
	result = FormatUptimeRFC3339("invalid")
	if result != "-" {
		t.Errorf("FormatUptimeRFC3339(\"invalid\") = %q, want \"-\"", result)
	}

	// Test valid timestamp (recent)
	recentTime := time.Now().Add(-5 * time.Minute).Format(time.RFC3339)
	result = FormatUptimeRFC3339(recentTime)
	if result == "-" {
		t.Errorf("FormatUptimeRFC3339(%q) = \"-\", expected valid uptime", recentTime)
	}
}
