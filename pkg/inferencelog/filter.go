package inferencelog

// Log-filter support for GET /zzrouter/v1/runs/:id/logs.
//
// When the caller supplies `grep=` or `regex=`, the logs handler builds
// a LogFilter and applies it to the stream (static tail or SSE follow)
// before emitting lines. The intent is to let AI agents and scripts
// fetch only the lines they need instead of pulling a large tail and
// filtering client-side — saving bandwidth and context tokens.
//
// Non-follow mode supports the full feature: substring/regex matching,
// `context=N` lines before and after each hit, and a `max_matches`
// cap with a `truncated` flag.
//
// Follow mode supports substring/regex matching only. Context windows
// in a live stream would require delaying `context_after` lines until
// we know whether a future line matches — that's deferred to v1.1.
// max_matches is also ignored in follow mode (streams are inherently
// unbounded).

import (
	"errors"
	"fmt"
	"regexp"
)

// LogFilterMaxContext is the maximum allowed value for the `context`
// query param. Kept small so a misconfigured client cannot force the
// server to buffer unbounded neighbor windows.
const LogFilterMaxContext = 20

// LogFilterDefaultMaxMatches is the default cap on matches returned
// from a non-follow grep query when the caller doesn't specify
// max_matches.
const LogFilterDefaultMaxMatches = 200

// LogFilterAbsoluteMaxMatches is the hard ceiling for max_matches
// regardless of caller input.
const LogFilterAbsoluteMaxMatches = 1000

// LogFilter is the compiled representation of a grep/regex query.
// A nil *LogFilter means "no filter active — emit everything".
type LogFilter struct {
	// re is the compiled regex used to match lines. For `grep=` this
	// is the literal substring wrapped in `(?i)` + QuoteMeta so we can
	// take a single matcher path and skip a per-line strings.ToLower
	// allocation on busy log files. For `regex=` it's the caller's
	// pattern compiled as-is.
	re *regexp.Regexp

	// context is the number of lines of context to include before and
	// after each match in non-follow output. 0 means no context.
	context int

	// maxMatches caps the number of matching lines returned in non-
	// follow mode. Ignored in follow mode. Zero or negative means
	// LogFilterDefaultMaxMatches.
	maxMatches int
}

// CompileLogFilter parses the raw query parameters into a LogFilter.
// Returns nil if neither grepStr nor regexStr is set. Returns an error
// if both are set or if regexStr fails to compile.
func CompileLogFilter(grepStr, regexStr string, ctx, maxMatches int) (*LogFilter, error) {
	if grepStr == "" && regexStr == "" {
		return nil, nil
	}
	if grepStr != "" && regexStr != "" {
		return nil, errors.New("grep and regex are mutually exclusive")
	}
	if ctx < 0 {
		ctx = 0
	}
	if ctx > LogFilterMaxContext {
		return nil, fmt.Errorf("context must be between 0 and %d", LogFilterMaxContext)
	}
	if maxMatches <= 0 {
		maxMatches = LogFilterDefaultMaxMatches
	}
	if maxMatches > LogFilterAbsoluteMaxMatches {
		maxMatches = LogFilterAbsoluteMaxMatches
	}

	f := &LogFilter{context: ctx, maxMatches: maxMatches}
	if grepStr != "" {
		// Literal substring, case-insensitive. QuoteMeta escapes any
		// regex metacharacters; (?i) flips the compiled matcher to
		// case-insensitive without per-line lowercasing.
		re, err := regexp.Compile("(?i)" + regexp.QuoteMeta(grepStr))
		if err != nil {
			return nil, fmt.Errorf("invalid grep pattern: %w", err)
		}
		f.re = re
		return f, nil
	}
	re, err := regexp.Compile(regexStr)
	if err != nil {
		return nil, fmt.Errorf("invalid regex: %w", err)
	}
	f.re = re
	return f, nil
}

// Match reports whether a single line satisfies the filter.
// A nil filter matches everything.
func (f *LogFilter) Match(line string) bool {
	if f == nil {
		return true
	}
	return f.re.MatchString(line)
}

// Apply filters a complete slice of lines with full context-window
// support and a max_matches cap. Used by the non-follow path.
//
// Algorithm:
//  1. Walk the input left-to-right, marking matching indices.
//  2. For each match within the cap, expand the window by `context`
//     lines on either side.
//  3. Deduplicate overlapping windows (a common case with dense logs).
//  4. Emit the lines in original order.
//
// Returns the filtered lines and whether the cap was reached.
func (f *LogFilter) Apply(lines []string) (filtered []string, truncated bool) {
	if f == nil {
		return lines, false
	}
	if len(lines) == 0 {
		return nil, false
	}

	// Collect matching indices up to the cap. truncated fires only
	// when we discover a match *beyond* the cap, so a response whose
	// hit count is exactly equal to max_matches is not marked
	// truncated (there were no more matches to drop).
	var hits []int
	for i, ln := range lines {
		if !f.Match(ln) {
			continue
		}
		if len(hits) >= f.maxMatches {
			truncated = true
			break
		}
		hits = append(hits, i)
	}
	if len(hits) == 0 {
		return nil, false
	}

	// Build a boolean mask covering each match + context window,
	// merging overlaps implicitly via the mask.
	include := make([]bool, len(lines))
	for _, idx := range hits {
		lo := idx - f.context
		if lo < 0 {
			lo = 0
		}
		hi := idx + f.context
		if hi >= len(lines) {
			hi = len(lines) - 1
		}
		for j := lo; j <= hi; j++ {
			include[j] = true
		}
	}

	// Emit preserved lines in order.
	out := make([]string, 0, len(hits)*(2*f.context+1))
	for i, ln := range lines {
		if include[i] {
			out = append(out, ln)
		}
	}
	return out, truncated
}
