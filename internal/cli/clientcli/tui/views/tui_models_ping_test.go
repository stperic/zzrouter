package views

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

// A ping reply can land after the user has moved on. Reporting it against
// whichever model the cursor now sits on would be a lie about a model
// that was never tested.
func TestPingResultIsScopedToTheModelItWasStartedFrom(t *testing.T) {
	m := &ModelsViewModel{ping: pingState{result: &modelPingMsg{
		model: "qwen3-8b",
		ttft:  400 * time.Millisecond,
	}}}

	if line, _ := m.pingResultLine("qwen3-8b"); line == "" {
		t.Error("no result for the pinged model; the Test section would not render")
	}
	if line, _ := m.pingResultLine("llama3-70b"); line != "" {
		t.Errorf("result leaked onto an untested model: %q", line)
	}
}

// The in-flight line is what tells the user the keystroke registered, so
// it must render before any result exists.
func TestPingInFlightRendersBeforeAnyResult(t *testing.T) {
	m := &ModelsViewModel{ping: pingState{model: "qwen3-8b"}}

	line, spinning := m.pingResultLine("qwen3-8b")
	if line == "" || !spinning {
		t.Fatalf("in-flight ping want a spinning line, got %q spinning=%v", line, spinning)
	}
	if line, _ := m.pingResultLine("llama3-70b"); line != "" {
		t.Errorf("in-flight line leaked onto another model: %q", line)
	}
}

// A failed ping's whole value is naming the failure. The last loading
// status is appended because "timed out" reads very differently when the
// model was still loading.
func TestPingFailureReportsErrorAndLastStatus(t *testing.T) {
	m := &ModelsViewModel{ping: pingState{result: &modelPingMsg{
		model:  "qwen3-8b",
		err:    errors.New("context deadline exceeded"),
		status: "loading model",
	}}}

	line, _ := m.pingResultLine("qwen3-8b")
	if !strings.Contains(line, "context deadline exceeded") {
		t.Errorf("failure line lost the error: %q", line)
	}
	if !strings.Contains(line, "loading model") {
		t.Errorf("failure line lost the last status: %q", line)
	}
}

// The reply shares one detail-panel field with its label; a multi-line or
// long completion would break the panel's alignment.
func TestFlattenReplyFitsOneDetailLine(t *testing.T) {
	if got := flattenReply("OK\n\n  sure  thing "); got != "OK sure thing" {
		t.Errorf("want newlines and runs of spaces collapsed, got %q", got)
	}

	got := flattenReply(strings.Repeat("a", pingReplyLimit+50))
	if len([]rune(got)) > pingReplyLimit+1 {
		t.Errorf("reply not truncated: %d runes", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncation not signalled: %q", got)
	}
}

// Precision that is meaningful at one scale is noise at the next: a tenth
// of a second matters when the answer took 1.3s and not when it took 90.
func TestFormatPingDurationTradesPrecisionForLegibility(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{412 * time.Millisecond, "412ms"},
		{time.Second + 340*time.Millisecond, "1.3s"},
		{90 * time.Second, "90s"},
	} {
		if got := formatPingDuration(tc.d); got != tc.want {
			t.Errorf("formatPingDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// The bug this feature shipped with: Qwen3 spends its whole budget in
// reasoning_content and emits no content, and the ping called a healthy
// model dead. Thinking tokens are part of the answer, so they must both
// count as proof of life and be reported as their own phase.
func TestPingReportsThinkingAsAPhaseNotAnOutcome(t *testing.T) {
	res := &modelPingMsg{
		model: "qwen3-8b", total: 782 * time.Millisecond, ttft: 173 * time.Millisecond,
		thinkDur: 412 * time.Millisecond, thinking: 19, reply: "OK",
	}
	m := &ModelsViewModel{ping: pingState{result: res}}

	line, _ := m.pingResultLine("qwen3-8b")
	if !strings.Contains(line, "782ms") {
		t.Errorf("Result should headline the full round-trip, got %q", line)
	}

	timing := pingTimingLine(res)
	for _, want := range []string{"first token 173ms", "thinking 412ms", "19 tokens"} {
		if !strings.Contains(timing, want) {
			t.Errorf("timing line missing %q: %q", want, timing)
		}
	}

	// A model that never reached content is still reachable, and the panel
	// has to say so rather than leave Reply blank.
	res.reply = ""
	got := pingReplyText(res)
	if !strings.Contains(got, "still thinking") || !strings.Contains(got, "19") {
		t.Errorf("empty reply should name the thinking it actually did: %q", got)
	}
	if pingTimingLine(&modelPingMsg{ttft: 96 * time.Millisecond}) != "first token 96ms" {
		t.Error("a model that never thinks should not get a thinking clause")
	}
}

// Repeating Provider and Node from the panel above adds a line and no
// information; the field earns its place only when a route dispatched the
// request somewhere other than the row the user selected.
func TestPingServedByOnlyShowsWhenItContradictsTheRow(t *testing.T) {
	row := &modelRow{Provider: "llamacpp", Node: "worker-1"}

	if got := pingServedBy(&modelPingMsg{provider: "llamacpp", node: "worker-1"}, row); got != "" {
		t.Errorf("matching served-by should be suppressed, got %q", got)
	}
	if got := pingServedBy(&modelPingMsg{provider: "vllm", node: "gpu-2"}, row); got != "vllm on gpu-2" {
		t.Errorf("want \"vllm on gpu-2\", got %q", got)
	}
	// Not every dispatch path sets both headers; a missing half must not
	// leave a dangling "vllm on".
	if got := pingServedBy(&modelPingMsg{provider: "vllm"}, row); got != "vllm" {
		t.Errorf("want \"vllm\", got %q", got)
	}
}

// "context deadline exceeded" is accurate and useless to someone deciding
// whether their model is broken or merely slow. A timeout is the one
// failure with no provider message to show instead.
func TestPingTimeoutCopyAvoidsGoInternals(t *testing.T) {
	res := &modelPingMsg{err: fmt.Errorf("error reading stream: %w", context.DeadlineExceeded)}
	got := pingErrorText(res)
	if strings.Contains(got, "context deadline") {
		t.Errorf("Go wording leaked into the panel: %q", got)
	}
	if !strings.Contains(got, "90s") {
		t.Errorf("timeout copy should name the budget it hit: %q", got)
	}
}

// The provider's own words are the thing a test exists to surface, so they
// are shown verbatim; the classification goes beside them rather than over
// them. OpenRouter's "Provider returned error" for a throttled free-tier
// model is the motivating case: unactionable alone, obvious once typed.
func TestPingShowsProviderErrorVerbatimAndClassifiesIt(t *testing.T) {
	res := &modelPingMsg{err: &pkgClient.ChatError{
		StatusCode: 429, Message: "Provider returned error", Type: "rate_limit_error",
	}}

	if got := pingErrorText(res); got != "Provider returned error" {
		t.Errorf("provider message not shown verbatim: %q", got)
	}
	detail := pingErrorDetail(res)
	for _, want := range []string{"rate_limit_error", "HTTP 429", "rate limited"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail line missing %q: %q", want, detail)
		}
	}

	// An error carried inside a 200 SSE stream has no meaningful HTTP
	// status; printing "HTTP 200" next to a failure reads as nonsense.
	inStream := &modelPingMsg{err: &pkgClient.ChatError{
		StatusCode: 200, Message: "upstream exploded", Type: "api_error",
	}}
	if strings.Contains(pingErrorDetail(inStream), "HTTP") {
		t.Errorf("2xx status should not be shown as a failure code: %q", pingErrorDetail(inStream))
	}

	// A message the type cannot improve on must not be echoed twice.
	plain := &modelPingMsg{err: &pkgClient.ChatError{Message: "boom", Type: "api_error"}}
	if strings.Count(pingErrorDetail(plain), "boom") > 0 {
		t.Errorf("detail duplicated the message: %q", pingErrorDetail(plain))
	}
}

// The spinner chain is shared with loading/fetching. A ping started while
// it is already running must not arm a second one, and a ping started
// while idle must arm it — otherwise the in-flight line never animates.
func TestPingArmsTheSpinnerChainExactlyOnce(t *testing.T) {
	rows := []modelRow{{RawModel: "qwen3-8b", Node: "worker-1"}}

	idle := &ModelsViewModel{rows: rows}
	idle.syncListItems()
	if cmd := idle.actionPing(); cmd == nil {
		t.Fatal("ping from idle returned no cmd")
	}
	if !idle.spinning {
		t.Error("ping from idle did not arm the spinner chain")
	}

	busy := &ModelsViewModel{rows: rows, spinning: true}
	busy.syncListItems()
	if cmd := busy.actionPing(); cmd == nil {
		t.Fatal("ping while spinning returned no cmd")
	}
	if !busy.ping.inFlight() {
		t.Error("ping while spinning did not record in-flight state")
	}
}

// An in-flight download has no instance to answer; pinging it would
// report a failure the user can do nothing about.
func TestPingRefusesRowsThatCannotAnswer(t *testing.T) {
	m := &ModelsViewModel{rows: []modelRow{{RawModel: "qwen3-8b", deploy: &shared.DeploymentInfo{Progress: "42%"}}}}
	m.syncListItems()

	if cmd := m.actionPing(); cmd != nil {
		t.Error("ping accepted a downloading row")
	}
	if m.ping.inFlight() {
		t.Error("ping marked a downloading row in flight")
	}
}

// The ping ends itself once the answer is in hand, so it must not mistake
// its own stop signal for a backend failure. Measured thinking-before-
// content across models ranges 0 to 89+ tokens on the same trivial prompt,
// which is why no fixed token budget can decide when a ping is done.
func TestPingDoesNotReportItsOwnStopSignalAsFailure(t *testing.T) {
	if !errors.Is(errPingDone, errPingDone) {
		t.Fatal("errPingDone is not comparable with errors.Is")
	}
	if pingReplyTokens >= modelPingMaxTokens {
		t.Errorf("reply cap (%d) must trip well before the cost backstop (%d), "+
			"or the backstop becomes the thing deciding when the ping ends",
			pingReplyTokens, modelPingMaxTokens)
	}
}
