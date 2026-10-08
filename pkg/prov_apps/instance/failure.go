package instance

import (
	"io"
	"os"
	"regexp"
	"strings"
	"unicode"

	"github.com/stperic/zzrouter/pkg/security"
	"github.com/stperic/zzrouter/pkg/utils"
)

const (
	failureTailLines = 32
	failureTailBytes = 8 * 1024
	// Read enough traceback context without loading an unbounded log.
	failureReadBytes    = 8 * failureTailBytes
	failureSummaryBytes = 1024
)

// FailureInfo is the bounded diagnostic snapshot retained by a failed run.
type FailureInfo struct {
	ExitCode  *int     `json:"exit_code"`
	Signal    string   `json:"signal,omitempty"`
	ErrorTail []string `json:"error_tail,omitempty"`
	Truncated bool     `json:"truncated"`
	specific  bool
}

// MarkFailedWithExit records process exit details without losing an earlier cause.
func (i *Instance) MarkFailedWithExit(reason string, exitCode *int, signal string) {
	failure, summary := CaptureFailure(i.LogFilePath, reason, exitCode, signal)
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.Status == StatusStopping || i.Status == StatusStopped {
		return
	}
	if i.failure == nil {
		i.failure = &FailureInfo{}
	}
	if i.Status != StatusFailed || i.ErrorMessage == "" || (!i.failure.specific && failure.specific) {
		i.ErrorMessage = summary
		i.failure.specific = failure.specific
	}
	if failure.ExitCode != nil {
		i.failure.ExitCode = failure.ExitCode
	}
	if failure.Signal != "" {
		i.failure.Signal = failure.Signal
	}
	if len(failure.ErrorTail) != 0 {
		i.failure.ErrorTail = failure.ErrorTail
	}
	i.failure.Truncated = i.failure.Truncated || failure.Truncated
	i.Status = StatusFailed
	if i.FailedAt.IsZero() {
		i.FailedAt = utils.Now()
	}
}

// CaptureFailure returns the bounded, redacted diagnostics shared by process owners.
func CaptureFailure(path, reason string, exitCode *int, signal string) (*FailureInfo, string) {
	tail, truncated := failureTail(path)
	summary := diagnosticText(reason)
	specific := false
	// Exception messages carry more information than traceback or liveness markers.
	cause := regexp.MustCompile(`\b[A-Za-z_][A-Za-z_0-9]*(?:Error|Exception):\s+\S.*`)
	for _, line := range tail {
		if found := cause.FindString(line); found != "" {
			summary, specific = found, true
			break
		}
	}
	if len(summary) > failureSummaryBytes {
		summary = summary[:failureSummaryBytes]
		truncated = true
	}
	failure := &FailureInfo{ErrorTail: tail, Truncated: truncated, specific: specific}
	if exitCode != nil {
		code := *exitCode
		failure.ExitCode = &code
	}
	if signal != "" {
		failure.Signal = diagnosticText(signal)
	}
	return failure, summary
}

func cloneFailure(f *FailureInfo) *FailureInfo {
	if f == nil {
		return nil
	}
	copy := *f
	copy.ErrorTail = append([]string(nil), f.ErrorTail...)
	if f.ExitCode != nil {
		code := *f.ExitCode
		copy.ExitCode = &code
	}
	return &copy
}

func failureTail(path string) ([]string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return nil, false
	}
	start := max(int64(0), stat.Size()-failureReadBytes)
	raw := make([]byte, min(stat.Size(), int64(failureReadBytes)))
	n, err := file.ReadAt(raw, start)
	if err != nil && err != io.EOF {
		return nil, false
	}
	text := string(raw[:n])
	truncated := start > 0
	if start > 0 {
		// A cut line could contain only the suffix of a credential.
		_, text, _ = strings.Cut(text, "\n")
	}
	var lines []string
	size := 0
	errorLine := regexp.MustCompile(`(?i)error|exception|traceback|fatal|failed`)
	payloadLine := regexp.MustCompile(`(?i)prompt|messages|request.?body|payload|environment|headers|non-default args`)
	for _, line := range strings.Split(text, "\n") {
		if !errorLine.MatchString(line) || payloadLine.MatchString(line) {
			continue
		}
		line = diagnosticText(line)
		if len(line) > failureTailBytes {
			truncated = true
			continue
		}
		lines = append(lines, line)
		size += len(line) + 1
		for len(lines) > failureTailLines || size > failureTailBytes {
			size -= len(lines[0]) + 1
			lines = lines[1:]
			truncated = true
		}
	}
	return lines, truncated
}

func diagnosticText(text string) string {
	ansi := regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|\x1b\\))`)
	text = ansi.ReplaceAllString(text, "")
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
	// URLs can carry credentials under arbitrary userinfo/query field names.
	urls := regexp.MustCompile(`https?://[^\s<>"']+`)
	text = urls.ReplaceAllString(text, "[REDACTED URL]")
	return strings.TrimSpace(string(security.RedactJSON([]byte(text))))
}
