package chat

import (
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui/views/search"

	"charm.land/glamour/v2"
	lipgloss "charm.land/lipgloss/v2"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ensureGlamourRenderer creates or recreates the glamour renderer if width changed
func (m *ChatModel) ensureGlamourRenderer() {
	st := m.state
	width := m.contentWidth()
	if st.glamourRenderer != nil && st.glamourWidth == width {
		return
	}

	r, err := glamour.NewTermRenderer(
		glamour.WithEnvironmentConfig(),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		st.glamourRenderer = nil
		return
	}
	st.glamourRenderer = r
	st.glamourWidth = width

	st.renderCache = make(map[int]string)
}

// contentWidth returns the width for glamour rendering.
// Accounts for left padding + glamour internal margins (blockquotes, code).
func (m *ChatModel) contentWidth() int {
	w := min(
		// left pad + glamour margins
		m.termWidth-len(chatLeftPad)-8, chatMaxTextWidth)
	if w < 40 {
		w = 40
	}
	return w
}

// renderMessage renders a single display message to a string
func (m *ChatModel) renderMessage(idx int) string {
	st := m.state
	msg := st.messages[idx]

	// Don't cache the actively streaming message
	isStreamingMsg := st.active && idx == len(st.messages)-1 && msg.Role == roleAssistant
	if !isStreamingMsg {
		if cached, ok := st.renderCache[idx]; ok {
			return cached
		}
	}

	var rendered string
	switch msg.Role {
	case roleUser:
		rendered = m.renderUserMessage(msg)
	case roleAssistant:
		rendered = m.renderAssistantMessage(msg, isStreamingMsg)
	case roleInfo:
		rendered = m.renderInfoMessage(msg)
	default:
		rendered = msg.Content
	}

	st.renderCache[idx] = rendered
	return rendered
}

// chatLeftPad is the consistent left margin for all chat content
const chatLeftPad = "  "

// renderUserMessage renders a user message with role indicator
func (m *ChatModel) renderUserMessage(msg chatDisplayMessage) string {
	label := m.styles.ChatUser.Render("You")
	// Indent content lines
	lines := strings.Split(msg.Content, "\n")
	for i, line := range lines {
		lines[i] = chatLeftPad + line
	}
	return chatLeftPad + label + "\n" + strings.Join(lines, "\n")
}

// renderAssistantMessage renders an assistant message with glamour markdown.
// During active streaming, raw text is shown to avoid O(n^2) glamour re-renders.
// Glamour formatting is applied once when the stream completes.
func (m *ChatModel) renderAssistantMessage(msg chatDisplayMessage, streaming bool) string {
	st := m.state
	var parts []string

	// Role label — show short model name (after last /)
	shortName := m.displayName
	if idx := strings.LastIndex(shortName, "/"); idx >= 0 {
		shortName = shortName[idx+1:]
	}
	var label string
	if streaming || (st.active && msg.Content == "") {
		// Shimmer effect while generating
		label = ui.Shimmer(shortName, st.animFrame, "#4ec98b")
	} else {
		label = m.styles.ChatAssistant.Render(shortName)
	}
	parts = append(parts, chatLeftPad+label)

	// Thinking block (completed message only, when /thinking is on)
	if msg.Thinking != "" && !streaming && !st.active && st.showThinking {
		thinkBox := m.styles.ChatThinkingBox.Render(
			m.styles.ChatThinking.Render("Thinking...") + "\n" +
				m.styles.ChatThinking.Render(msg.Thinking),
		)
		parts = append(parts, thinkBox)
	}

	// Content: raw text while streaming, glamour-rendered after completion
	content := msg.Content
	if content != "" {
		if !streaming && st.glamourRenderer != nil {
			if rendered, err := st.glamourRenderer.Render(content); err == nil {
				content = strings.Trim(rendered, "\n")
			}
		} else if streaming {
			// Word-wrap raw streaming text, then indent
			wrapped := lipgloss.Wrap(content, m.contentWidth(), "")
			lines := strings.Split(wrapped, "\n")
			for i, line := range lines {
				lines[i] = chatLeftPad + line
			}
			content = strings.Join(lines, "\n")
		}
		parts = append(parts, content)
	} else if st.active {
		// Streaming placeholder — animated "thinking" or spinner
		if st.thinkingStarted {
			parts = append(parts, m.styles.ChatDim.Render("  "+shared.SpinnerFrame(st.animFrame)+" ")+ui.Shimmer("thinking", st.animFrame, "#888888"))
		} else {
			parts = append(parts, m.styles.ChatDim.Render("  "+shared.SpinnerFrame(st.animFrame)))
		}
	}

	// Stats
	if msg.Stats != nil && st.showStats {
		parts = append(parts, m.formatStats(msg.Stats))
	}

	return strings.Join(parts, "\n")
}

// renderInfoMessage renders an info/status message with word wrapping
func (m *ChatModel) renderInfoMessage(msg chatDisplayMessage) string {
	w := max(m.termWidth-len(chatLeftPad)-2, 40)
	wrapped := shared.WrapText(msg.Content, w)
	// Style and indent each line individually to preserve left alignment
	lines := strings.Split(wrapped, "\n")
	for i, line := range lines {
		lines[i] = chatLeftPad + m.styles.ChatDim.Render(line)
	}
	return strings.Join(lines, "\n")
}

// renderAllMessages builds the full viewport content
func (m *ChatModel) renderAllMessages() string {
	m.ensureGlamourRenderer()

	st := m.state
	var parts []string
	for i := range st.messages {
		rendered := m.renderMessage(i)
		if rendered != "" {
			parts = append(parts, rendered)
		}
	}

	// Join with blank line between messages, add trailing blank line for breathing room above status bar
	return strings.Join(parts, "\n\n") + "\n"
}

// invalidateStreamingMessage clears the cache for the currently streaming message
func (m *ChatModel) invalidateStreamingMessage() {
	st := m.state
	if len(st.messages) > 0 {
		idx := len(st.messages) - 1
		delete(st.renderCache, idx)
	}
}

// formatStats formats performance stats for display
func (m *ChatModel) formatStats(s *chatMsgStats) string {
	if s.TotalTokens > 0 && !s.IsEstimate {
		var parts []string

		// Prompt portion with cache info
		if s.CachedTokens > 0 {
			parts = append(parts, fmt.Sprintf("%d prompt (%d cached, %.0fms, %.0f t/s)",
				s.PromptTokens, s.CachedTokens, s.PrefillMs, s.PrefillSpeed))
		} else {
			parts = append(parts, fmt.Sprintf("%d prompt (%.0fms, %.0f t/s)",
				s.PromptTokens, s.PrefillMs, s.PrefillSpeed))
		}

		// Generation portion with reasoning breakdown
		genLabel := fmt.Sprintf("%d gen (%.1fs, %.1f t/s)", s.CompletionTokens, s.DecodeSeconds, s.DecodeSpeed)
		if s.ReasoningTokens > 0 {
			genLabel = fmt.Sprintf("%d gen (%d reasoning, %.1fs, %.1f t/s)",
				s.CompletionTokens, s.ReasoningTokens, s.DecodeSeconds, s.DecodeSpeed)
		}
		parts = append(parts, genLabel)

		// Total
		parts = append(parts, fmt.Sprintf("%d total", s.TotalTokens))

		// Cost
		if s.Cost > 0 {
			parts = append(parts, formatCost(s.Cost))
		}

		return m.styles.ChatDim.Render("  " + strings.Join(parts, " │ "))
	}

	return m.styles.ChatDim.Render(fmt.Sprintf(
		"  ~%d prompt (%.0fms) │ ~%d gen (%.1fs, %.1f t/s) │ ~%d total",
		s.PromptTokens, s.PrefillMs,
		s.CompletionTokens, s.DecodeSeconds, s.DecodeSpeed,
		s.TotalTokens,
	))
}

// formatCost formats a USD cost value for display
func formatCost(cost float64) string {
	if cost < 0.001 {
		return fmt.Sprintf("$%.4f", cost)
	}
	if cost < 0.01 {
		return fmt.Sprintf("$%.3f", cost)
	}
	return fmt.Sprintf("$%.2f", cost)
}

// processStreamChunk handles the Hermes 3 tag state machine
func (m *ChatModel) processStreamChunk(chunk pkgClient.StreamChunk) {
	st := m.state

	if chunk.IsStatusUpdate {
		st.statusMessage = chunk.Content
		if chunk.StatusType == "model_ready" {
			st.statusMessage = ""
			st.modelLoaded = true
		}
		return
	}

	if chunk.TotalTokens > 0 {
		st.promptTokens = chunk.PromptTokens
		st.completionTokens = chunk.CompletionTokens
		st.totalTokens = chunk.TotalTokens
		st.cachedTokens = chunk.CachedTokens
		st.reasoningTokens = chunk.ReasoningTokens
		st.cost = chunk.Cost
		return
	}

	if !st.firstTokenRecv && (chunk.Content != "" || chunk.IsThinking) {
		st.modelLoaded = true
		st.firstTokenTime = timeNow()
		st.firstTokenRecv = true
	}

	// Update display name with resolved routing info from backend response.
	// Rebuilds on each response so route load-balancing shows the current deployment.
	// Cloud:  "gemini-2.5-flash (gemini)"
	// Local:  "qwen3-coder:latest (ollama on macbook-pro)"
	if chunk.Model != "" && chunk.Provider != "" {
		suffix := chunk.Provider
		isCloud := search.DefaultSearchProviders[chunk.Provider].IsCloud
		if chunk.Node != "" && !isCloud {
			suffix += " on " + chunk.Node
		}
		m.displayName = chunk.Model + " (" + suffix + ")"
	}

	if chunk.IsThinking {
		st.thinkingStarted = true
		st.thinking.WriteString(chunk.Reasoning)
		m.updateStreamingDisplayMessage()
		return
	}

	if chunk.Content != "" {
		st.buffer.WriteString(chunk.Content)
		m.processHermesTags()
	}
}

// Hermes 3 channel tag constants
const (
	hermesAnalysisTag = "<|channel|>analysis<|message|>"
	hermesFinalTag    = "<|channel|>final<|message|>"
	hermesEndTag      = "<|end|>"
)

// processHermesTags runs the Hermes 3 tag state machine
func (m *ChatModel) processHermesTags() {
	st := m.state
	text := st.buffer.String()

	for {
		switch st.hermesState {
		case hermesNormal:
			if idx := strings.Index(text, hermesAnalysisTag); idx >= 0 {
				if idx > 0 {
					st.content.WriteString(text[:idx])
				}
				st.hermesState = hermesInThinking
				st.thinkingStarted = true
				text = text[idx+len(hermesAnalysisTag):]
				continue
			} else if idx := strings.Index(text, hermesFinalTag); idx >= 0 {
				if idx > 0 {
					st.content.WriteString(text[:idx])
				}
				st.hermesState = hermesInFinal
				text = text[idx+len(hermesFinalTag):]
				continue
			} else if strings.Contains(text, "<|") {
				st.buffer.Reset()
				st.buffer.WriteString(text)
				m.updateStreamingDisplayMessage()
				return
			}
			st.content.WriteString(text)
			st.buffer.Reset()
			m.updateStreamingDisplayMessage()
			return

		case hermesInThinking:
			if idx := strings.Index(text, hermesEndTag); idx >= 0 {
				st.thinking.WriteString(text[:idx])
				text = text[idx+len(hermesEndTag):]
				st.hermesState = hermesNormal
				continue
			} else if strings.Contains(text, "<|") {
				st.buffer.Reset()
				st.buffer.WriteString(text)
				m.updateStreamingDisplayMessage()
				return
			}
			st.thinking.WriteString(text)
			st.buffer.Reset()
			m.updateStreamingDisplayMessage()
			return

		case hermesInFinal:
			if idx := strings.Index(text, hermesEndTag); idx >= 0 {
				st.content.WriteString(text[:idx])
				text = text[idx+len(hermesEndTag):]
				st.hermesState = hermesNormal
				continue
			} else if strings.Contains(text, "<|") {
				st.buffer.Reset()
				st.buffer.WriteString(text)
				m.updateStreamingDisplayMessage()
				return
			}
			st.content.WriteString(text)
			st.buffer.Reset()
			m.updateStreamingDisplayMessage()
			return
		}
	}
}

// updateStreamingDisplayMessage updates the last message with current streaming content
func (m *ChatModel) updateStreamingDisplayMessage() {
	st := m.state
	if len(st.messages) == 0 {
		return
	}
	idx := len(st.messages) - 1
	st.messages[idx].Content = st.content.String()
	st.messages[idx].Thinking = st.thinking.String()
	m.invalidateStreamingMessage()
}

// timeNow is a variable for testing
var timeNow = utils.Now
