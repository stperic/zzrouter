package metadata

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	quantRegex       = regexp.MustCompile(`(?i)Q([2-8])_([A-Z0-9_]+)`)
	quantRegexSimple = regexp.MustCompile(`(?i)Q([2-8])_([0-9])`)
)

// ExtractQuantization extracts the GGUF quantization tag from a
// filename. Examples: "model-Q4_K_M.gguf" → "Q4_K_M", "model-Q8_0.gguf"
// → "Q8_0". Returns "" when no GGUF quantization pattern is present.
// Pure string operation; lives in the metadata leaf so source sub-
// packages and external callers can share one implementation.
func ExtractQuantization(filename string) string {
	matches := quantRegex.FindStringSubmatch(filename)
	if len(matches) >= 3 {
		quant := fmt.Sprintf("Q%s_%s", matches[1], matches[2])
		return strings.ToUpper(quant)
	}

	// Fallback: simpler pattern Q[2-8]_[0-9] for Q4_0, Q5_0, Q8_0.
	matchesSimple := quantRegexSimple.FindStringSubmatch(filename)
	if len(matchesSimple) >= 3 {
		return fmt.Sprintf("Q%s_%s", matchesSimple[1], matchesSimple[2])
	}

	return ""
}
