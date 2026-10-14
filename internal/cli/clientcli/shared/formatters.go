package shared

import (
	"fmt"
	"strings"
	"time"

	pkgUtils "github.com/stperic/zzrouter/pkg/utils"
)

// FormatDuration formats a duration as a human-readable relative time string (Ollama-compatible)
func FormatDuration(duration time.Duration, isFuture bool) string {
	if duration < time.Minute {
		if isFuture {
			return "less than a minute"
		}
		return "just now"
	}

	if duration < time.Hour {
		return FormatTimeUnit(duration, time.Minute, "minute", isFuture)
	}

	if duration < 24*time.Hour {
		return FormatTimeUnit(duration, time.Hour, "hour", isFuture)
	}

	if duration < 14*24*time.Hour {
		return FormatTimeUnit(duration, 24*time.Hour, "day", isFuture)
	}

	if duration < 30*24*time.Hour {
		return FormatTimeUnit(duration, 7*24*time.Hour, "week", isFuture)
	}

	months := int(duration.Hours() / (24 * 30))
	if months == 1 {
		return "1 month ago"
	}
	return fmt.Sprintf("%d months ago", months)
}

// FormatTimeUnit formats a duration in terms of a specific time unit
func FormatTimeUnit(duration, unit time.Duration, unitName string, isFuture bool) string {
	count := int(duration / unit)
	if count == 1 {
		if isFuture {
			return fmt.Sprintf("1 %s from now", unitName)
		}
		return fmt.Sprintf("1 %s ago", unitName)
	}

	if isFuture {
		return fmt.Sprintf("%d %ss from now", count, unitName)
	}
	return fmt.Sprintf("%d %ss ago", count, unitName)
}

// FormatSize is a wrapper around the centralized FormatSize utility
func FormatSize(size int64) string {
	if size == 0 {
		return EmptyValue
	}
	return pkgUtils.FormatSize(size)
}

// FormatSpeed formats speed in bytes per second to human-readable format
func FormatSpeed(speed int64) string {
	if speed == 0 {
		return "N/A"
	}
	return fmt.Sprintf("%s/s", FormatSize(speed))
}

// FormatCloudPrice formats input/output pricing per million tokens.
func FormatCloudPrice(promptPerToken, completePerToken float64) string {
	if promptPerToken == 0 && completePerToken == 0 {
		return EmptyValue
	}
	inPerM := promptPerToken * 1_000_000
	outPerM := completePerToken * 1_000_000
	fmtPrice := func(p float64) string {
		if p < 0.01 {
			return fmt.Sprintf("$%.3f", p)
		}
		return fmt.Sprintf("$%.2f", p)
	}
	return fmt.Sprintf("%s/%s", fmtPrice(inPerM), fmtPrice(outPerM))
}

// FormatContextWindow formats large token counts with K/M suffixes.
func FormatContextWindow(tokens int) string {
	if tokens >= 1_000_000 {
		return fmt.Sprintf("%.0fM", float64(tokens)/1_000_000)
	}
	if tokens >= 1000 {
		return fmt.Sprintf("%.0fK", float64(tokens)/1000)
	}
	return fmt.Sprintf("%d", tokens)
}

// FormatNoModelsFoundMessage creates a consistent "no models found" message
func FormatNoModelsFoundMessage(node, provider, model string) string {
	var parts []string

	if node != "" && node != "*" {
		parts = append(parts, fmt.Sprintf("on node '%s'", node))
	}

	if provider != "" && provider != "*" {
		parts = append(parts, fmt.Sprintf("with provider '%s'", provider))
	}

	if model != "" {
		parts = append(parts, fmt.Sprintf("matching '%s'", model))
	}

	if len(parts) == 0 {
		return "No models found"
	}

	return "No models found " + strings.Join(parts, " ")
}

// FormatRPM formats a requests-per-minute limit for list display.
func FormatRPM(limit int) string {
	if limit <= 0 {
		return EmptyValue
	}
	return fmt.Sprintf("%d", limit)
}

// FormatBudget formats a spend limit for list display.
func FormatBudget(limit float64, period string) string {
	if limit <= 0 {
		return EmptyValue
	}
	if period == "" {
		period = "monthly"
	}
	return fmt.Sprintf("$%.0f/%s", limit, period)
}

// RenderBudgetBar renders a progress bar for budget spend percentage.
func RenderBudgetBar(pct float64, maxWidth int) string {
	if maxWidth < 10 {
		maxWidth = 10
	}
	barWidth := min(maxWidth, 40)

	filled := min(int(pct/100*float64(barWidth)), barWidth)
	if filled < 0 {
		filled = 0
	}

	bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
	return fmt.Sprintf("%s  %.1f%%", bar, pct)
}

// FormatTokenCount formats large token counts with comma separators.
func FormatTokenCount(n int64) string {
	if n == 0 {
		return "0"
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}

	var result strings.Builder
	remainder := len(s) % 3
	if remainder > 0 {
		result.WriteString(s[:remainder])
		if len(s) > remainder {
			result.WriteString(",")
		}
	}
	for i := remainder; i < len(s); i += 3 {
		if i > remainder {
			result.WriteString(",")
		}
		result.WriteString(s[i : i+3])
	}
	return result.String()
}

// FormatModelWithRegistry prefixes a model name with its registry if present.
func FormatModelWithRegistry(registry, model string) string {
	if registry == "" {
		return model
	}
	return registry + ":" + model
}

// ErrorPrefixes are stripped from nested error messages.
var ErrorPrefixes = []string{
	"failed to initiate download: ",
	"download failed: ",
	"Failed to start download: ",
	"failed to ",
	"error: ",
	"Error: ",
}

// ExtractUserFriendlyError strips nested error prefixes to show clean user message.
func ExtractUserFriendlyError(err error) string {
	if err == nil {
		return ""
	}

	msg := err.Error()

	changed := true
	for changed {
		changed = false
		for _, prefix := range ErrorPrefixes {
			if strings.HasPrefix(msg, prefix) {
				msg = msg[len(prefix):]
				changed = true
				break
			}
		}
	}

	if len(msg) > 0 {
		first := string(msg[0])
		if first == strings.ToLower(first) && first != "'" && first != "\"" {
			msg = strings.ToUpper(first) + msg[1:]
		}
	}

	return msg
}

// FormatSearchDownloads formats download counts with K/M suffixes.
func FormatSearchDownloads(count int) string {
	if count >= 1000000 {
		return fmt.Sprintf("%.1fM", float64(count)/1000000)
	} else if count >= 1000 {
		return fmt.Sprintf("%.1fK", float64(count)/1000)
	}
	return fmt.Sprintf("%d", count)
}

// FormatUptimeDuration formats seconds into a compact uptime string.
func FormatUptimeDuration(seconds int) string {
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := seconds / 60
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}
	hours := minutes / 60
	mins := minutes % 60
	if hours < 24 {
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	days := hours / 24
	hrs := hours % 24
	return fmt.Sprintf("%dd %dh", days, hrs)
}
