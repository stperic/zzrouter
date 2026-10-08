package views

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Model ping — one round-trip through the real inference path.
//
// Instance probes answer "is the process up"; they say nothing about
// whether a token ever comes back out. The two states differ constantly:
// an engine that OOMs on the first decode, a cloud key that expired, a
// route whose only deployment is cold. Until now the only way to tell
// them apart was to open the chat view and type.
//
// The ping is a chat completion with a tiny budget — not a new protocol.
// It goes through the same /v1/chat/completions the chat view uses, so a
// green result is proof the next real request will work, not proof that
// some parallel health path is happy.

const (
	// modelPingTimeout has to cover a cold start: the server accepts the
	// request, loads weights, and streams loading_model status events
	// meanwhile. A warm instance answers in well under a second.
	modelPingTimeout = 90 * time.Second

	// modelPingMaxTokens is a cost backstop, NOT what decides when the ping
	// is done. Thinking length before the first content token varies wildly
	// across models on the same trivial prompt — measured here: gemini-flash
	// 0, deepseek-v4 5, Qwen3 18, glm-latest 46, nemotron-lightning 89, and
	// an o1/R1-class reasoner runs into the hundreds. Any budget small
	// enough to be cheap is small enough to cut a healthy model off
	// mid-thought and report it as silent, which is the bug this constant
	// used to cause. The ping stops itself at the first content tokens
	// instead (see pingReplyTokens), bounded by modelPingTimeout; this only
	// caps a model that never stops thinking at all.
	modelPingMaxTokens = 2048

	// pingReplyTokens is how much of the answer to collect once content
	// starts. Enough to show the model formed words, then abort the stream
	// rather than pay for a completion nobody will read.
	pingReplyTokens = 24

	modelPingPrompt = "Reply with OK."
)

// modelPingMsg is the outcome of one ping.
//
// Model is echoed back because the reply can land after the user has
// moved to another row — the handler drops a result that no longer
// matches what is on screen rather than mislabeling it.
type modelPingMsg struct {
	model string
	// ttft is time to the first token of any kind, measured from before
	// the request — so on a cold model it includes the load, which is
	// exactly the wait the user is asking about.
	ttft time.Duration
	// total is the full round-trip, to the end of the stream.
	total time.Duration
	// thinkDur is how long the model spent between its first reasoning
	// token and its first content token. Thinking is a phase of the
	// answer, not an outcome: a reasoning model is mid-response here, and
	// folding this into total would hide where the time actually went.
	thinkDur time.Duration
	// thinking counts chain-of-thought tokens. It stays non-zero even when
	// the model never reaches content, which is what proves a reasoning
	// model answered at all.
	thinking int
	reply    string
	provider string
	node     string
	// status is the last loading_model event seen. Only interesting when
	// the ping fails: it distinguishes "never started loading" from
	// "still loading when we gave up".
	status string
	err    error
}

// pingState is the view's ping slot. One in flight at a time — the
// detail panel shows one model, so a second ping would have nowhere to
// render.
type pingState struct {
	model  string // non-empty while a ping is in flight
	result *modelPingMsg
}

func (p pingState) inFlight() bool { return p.model != "" }

// errPingDone ends the stream once the answer is in hand. Returning an
// error is how the streaming client is told to stop; it is filtered out
// rather than reported.
var errPingDone = errors.New("ping: answer received")

// actionPing sends a minimal completion to the selected model and reports
// what came back.
func (m *ModelsViewModel) actionPing() tea.Cmd {
	r := m.selectedRow()
	if r == nil || r.deploy != nil {
		return nil
	}

	client := m.client
	model := r.RawModel
	node := r.Node
	if node == constants.CloudNodeName {
		node = ""
	}

	m.ping = pingState{model: model}
	cmd := func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), modelPingTimeout)
		defer cancel()

		out := modelPingMsg{model: model}
		start := utils.Now()
		var thinkStart, thinkEnd time.Time
		var contentChunks int
		err := client.ChatCompletionsStream(ctx, pkgClient.ChatRequest{
			Model:         model,
			Messages:      []pkgClient.ChatMessage{{Role: "user", Content: modelPingPrompt}},
			MaxTokens:     modelPingMaxTokens,
			PreferredNode: node,
		}, func(chunk pkgClient.StreamChunk) error {
			if chunk.IsStatusUpdate {
				out.status = chunk.Content
				return nil
			}
			if chunk.Content == "" && chunk.Reasoning == "" {
				return nil
			}
			now := utils.Now()
			if out.ttft == 0 {
				out.ttft = now.Sub(start)
			}
			if chunk.Reasoning != "" {
				out.thinking++
				if thinkStart.IsZero() {
					thinkStart = now
				}
			}
			if chunk.Content != "" {
				// First content token closes the thinking phase.
				if !thinkStart.IsZero() && thinkEnd.IsZero() {
					thinkEnd = now
				}
				out.reply += chunk.Content
				contentChunks++
			}
			// Provider and node ride on every token chunk; the last one is
			// as good as the first and saves a branch.
			out.provider, out.node = chunk.Provider, chunk.Node
			if contentChunks >= pingReplyTokens {
				return errPingDone
			}
			return nil
		})
		out.total = utils.Now().Sub(start)

		// A model still thinking when the stream ended never opened its
		// content phase; charge the remainder to thinking rather than
		// dropping it.
		if !thinkStart.IsZero() {
			if thinkEnd.IsZero() {
				thinkEnd = utils.Now()
			}
			out.thinkDur = thinkEnd.Sub(thinkStart)
		}

		switch {
		case err != nil && !errors.Is(err, errPingDone):
			out.err = err
		case out.reply == "" && out.thinking == 0:
			// HTTP 200 with no tokens at all is a failed ping: this is
			// exactly the silent hang the chat view would show.
			out.err = errors.New("accepted the request but produced no output")
		}
		return out
	}

	// The spinner chain is shared with the loading/fetching states; arm it
	// only if it is not already running.
	if m.spinning {
		return cmd
	}
	m.spinning = true
	return tea.Batch(cmd, shared.SpinnerTickCmd())
}

// pingResultLine renders the Result field of the Test section of the detail panel. Returns nothing
// when no ping has been run for this model, so the panel does not grow a
// permanently empty section.
func (m *ModelsViewModel) pingResultLine(model string) (string, bool) {
	if m.ping.inFlight() && m.ping.model == model {
		return "sending test prompt…", true
	}
	res := m.ping.result
	if res == nil || res.model != model {
		return "", false
	}
	if res.err != nil {
		msg := pingErrorText(res)
		if res.status != "" {
			msg = fmt.Sprintf("%s (last status: %s)", msg, res.status)
		}
		return "✗ " + msg, false
	}
	return "✓ responded in " + formatPingDuration(res.total), false
}

// pingTimingLine breaks the round-trip into the phases the user can act
// on. Thinking is one of those phases, not a separate outcome, so it sits
// on the same line as the rest of the latency rather than above the reply.
func pingTimingLine(res *modelPingMsg) string {
	parts := []string{"first token " + formatPingDuration(res.ttft)}
	if res.thinking > 0 {
		parts = append(parts, fmt.Sprintf("thinking %s (%d tokens)",
			formatPingDuration(res.thinkDur), res.thinking))
	}
	return strings.Join(parts, "  ·  ")
}

// formatPingDuration trades precision for legibility as the number grows:
// "412ms", then "1.3s", then "90s". A tenth of a second is meaningful at
// one scale and noise at the next.
func formatPingDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
}

// pingReplyLimit truncates the echoed completion. The reply is there to
// confirm words came out, not to be read.
const pingReplyLimit = 120

// flattenReply collapses the completion onto the single line the detail
// panel gives it.
func flattenReply(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > pingReplyLimit {
		return s[:pingReplyLimit] + "…"
	}
	return s
}

// appendPingSection renders the detail panel's Test section. Nothing is
// emitted until a ping has been run for this model, so the panel does not
// carry a permanently empty section.
func (m *ModelsViewModel) appendPingSection(d *shared.DetailBuilder, row *modelRow) {
	model := row.RawModel
	line, spinning := m.pingResultLine(model)
	if line == "" {
		return
	}

	d.Section("Test")
	if spinning {
		line = shared.SpinnerFrame(m.loadTick) + " " + line
	}
	d.Field("Result", line)

	res := m.ping.result
	if res == nil || res.model != model {
		return
	}
	if res.err != nil {
		if detail := pingErrorDetail(res); detail != "" {
			d.Field("Detail", detail)
		}
		return
	}
	d.Field("Timing", pingTimingLine(res))
	d.Field("Reply", pingReplyText(res))
	// Only worth a line when it contradicts the panel above it: on a route,
	// the deployment that answered can differ from the row the user
	// selected. Echoing a match would just repeat Provider and Node.
	if servedBy := pingServedBy(res, row); servedBy != "" {
		d.Field("Served by", servedBy)
	}
}

// applyPingResult files a completed ping. A reply that outlived the
// selection it was started from has nothing to label, so it is dropped
// rather than reported against whichever model the cursor has since
// landed on.
func (m *ModelsViewModel) applyPingResult(msg modelPingMsg) {
	if m.ping.model != msg.model {
		return
	}
	m.ping = pingState{result: &msg}
}

// pingReplyText describes what came back. A reasoning model that spent the
// whole token budget thinking has answered — reporting a blank Reply there
// would read as the failure this ping exists to rule out.
func pingReplyText(res *modelPingMsg) string {
	if reply := flattenReply(res.reply); reply != "" {
		return reply
	}
	return fmt.Sprintf("(none: still thinking after %d tokens)", res.thinking)
}

// pingServedBy names who answered, and only when that is news. Either half
// can be missing (not every dispatch path sets both response headers), so
// the parts are joined defensively rather than blindly — a dangling
// "llamacpp on" reads as a truncated line.
func pingServedBy(res *modelPingMsg, row *modelRow) string {
	provider, node := res.provider, res.node
	if provider == row.Provider {
		provider = ""
	}
	if node == row.Node {
		node = ""
	}
	switch {
	case provider != "" && node != "":
		return provider + " on " + node
	case provider != "":
		return provider
	default:
		return node
	}
}

// pingErrorText keeps Go's wording out of the panel. "context deadline
// exceeded" is accurate and means nothing to someone deciding whether
// their model is broken or just slow.
func pingErrorText(res *modelPingMsg) string {
	// A timeout produces no provider message at all, so this is the one
	// case where Go's wording would otherwise reach the panel.
	if errors.Is(res.err, context.DeadlineExceeded) {
		return "no response within " + formatPingDuration(modelPingTimeout)
	}
	// Everything else is reported verbatim. The whole point of a test is
	// to see what the provider actually said; paraphrasing it here would
	// throw away the string the user needs to search for. The
	// interpretation goes on the Detail line instead.
	return res.err.Error()
}

// pingErrorDetail classifies the failure alongside the provider's own
// words. A message like OpenRouter's bare "Provider returned error" is
// unactionable on its own; the type says whether to wait or to go look at
// the configuration.
func pingErrorDetail(res *modelPingMsg) string {
	var chatErr *pkgClient.ChatError
	if !errors.As(res.err, &chatErr) {
		return ""
	}

	var parts []string
	if chatErr.Type != "" {
		parts = append(parts, chatErr.Type)
	}
	// Only when the HTTP layer itself failed. An error carried inside a
	// 200 SSE stream would otherwise render as "HTTP 200".
	if chatErr.StatusCode >= 400 {
		parts = append(parts, fmt.Sprintf("HTTP %d", chatErr.StatusCode))
	}
	if explain := chatErr.Explain(); explain != chatErr.Message {
		parts = append(parts, explain)
	}
	return strings.Join(parts, "  ·  ")
}
