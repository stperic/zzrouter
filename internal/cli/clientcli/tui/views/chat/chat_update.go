package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
)

// formatChatError turns a raw error into a user-friendly chat message.
func formatChatError(err error) string {
	var chatErr *pkgClient.ChatError
	if errors.As(err, &chatErr) {
		switch chatErr.StatusCode {
		case 400:
			return fmt.Sprintf("Could not run model: %s", chatErr.Message)
		case 404:
			return fmt.Sprintf("Model not found: %s", chatErr.Message)
		case 503:
			return fmt.Sprintf("Server unavailable: %s", chatErr.Message)
		default:
			return fmt.Sprintf("Server error (%d): %s", chatErr.StatusCode, chatErr.Message)
		}
	}
	return fmt.Sprintf("Connection error: %v", err)
}

// Update implements tea.Model — handles all events and messages
func (m *ChatModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m.handleWindowSize(msg)

	case tea.KeyPressMsg:
		return m.handleKeyPress(msg)

	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			m.viewport.ScrollUp(3)
			m.state.followMode = false
		case tea.MouseWheelDown:
			m.viewport.ScrollDown(3)
			if m.viewport.AtBottom() {
				m.state.followMode = true
			}
		}
		return m, nil

	case streamChunkMsg:
		return m.handleStreamChunk(msg)

	case streamDoneMsg:
		return m.handleStreamDone(msg)

	case ui.ShimmerTickMsg:
		if m.state.active {
			m.state.animFrame++
			m.refreshViewport()
			return m, ui.ShimmerTickCmd()
		}
		return m, nil
	}

	// Forward to textarea and viewport
	var taCmd tea.Cmd
	m.textarea, taCmd = m.textarea.Update(msg)
	m.resizeTextarea()

	if taCmd != nil {
		cmds = append(cmds, taCmd)
	}

	var vpCmd tea.Cmd
	m.viewport, vpCmd = m.viewport.Update(msg)
	if vpCmd != nil {
		cmds = append(cmds, vpCmd)
	}

	return m, tea.Batch(cmds...)
}

// handleWindowSize processes terminal resize events
func (m *ChatModel) handleWindowSize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	m.termWidth = msg.Width
	m.termHeight = msg.Height

	vpHeight := m.viewportHeight()

	// Set textarea width on every resize (including first)
	m.textarea.SetWidth(msg.Width - 4)

	if !m.ready {
		m.viewport = viewport.New(viewport.WithWidth(msg.Width), viewport.WithHeight(vpHeight))
		m.viewport.SetContent(m.renderAllMessages())
		m.ready = true
		if m.state.followMode {
			m.viewport.GotoBottom()
		}
		return m, nil
	}

	m.viewport.SetWidth(msg.Width)
	m.viewport.SetHeight(vpHeight)

	// Recreate glamour renderer if width changed
	oldWidth := m.state.glamourWidth
	m.ensureGlamourRenderer()
	if m.state.glamourWidth != oldWidth {
		m.refreshViewport()
	}

	return m, nil
}

// handleKeyPress processes keyboard input
func (m *ChatModel) handleKeyPress(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) { //nolint:gocyclo,cyclop // The explicit key, message and rendering cases keep the view flow together.
	ac := &m.state.autocomplete

	switch {
	case key.Matches(msg, tui.ChatKeys.Cancel):
		if m.state.active {
			m.state.active = false
			if m.state.cancelStream != nil {
				m.state.cancelStream()
				m.state.cancelStream = nil
			}
			m.state.ch = nil
			m.finishAssistantMessage()
			m.refreshViewport()
			return m, nil
		}
		return m, m.chatQuitOrBack()

	case key.Matches(msg, tui.ChatKeys.Back):
		return m, m.chatQuitOrBack()

	case key.Matches(msg, tui.ChatKeys.Copy):
		result := chatCopyCommand(m)
		if result.output != "" {
			m.state.messages = append(m.state.messages, chatDisplayMessage{
				Role:    roleInfo,
				Content: result.output,
			})
			m.refreshViewport()
		}
		return m, nil

	case key.Matches(msg, tui.ChatKeys.Esc):
		if ac.active {
			ac.reset()
			m.viewport.SetHeight(m.viewportHeight())
			return m, nil
		}
		// Empty input → go back; otherwise clear the input
		if strings.TrimSpace(m.textarea.Value()) == "" {
			return m, m.chatQuitOrBack()
		}
		m.textarea.Reset()
		ac.reset()
		m.viewport.SetHeight(m.viewportHeight())
		return m, nil

	case key.Matches(msg, tui.ChatKeys.Send):
		if ac.active {
			cmd := ac.selectedCommand()
			if cmd != "" {
				if ac.selectedHasArgs() {
					// Commands with args: insert text for user to complete
					m.textarea.Reset()
					m.textarea.InsertString(cmd + " ")
					ac.reset()
					m.viewport.SetHeight(m.viewportHeight())
					m.resizeTextarea()
					return m, nil
				}
				// Toggle/action commands: execute immediately
				m.textarea.Reset()
				ac.reset()
				m.viewport.SetHeight(m.viewportHeight())
				m.resizeTextarea()
				result := executeChatCommand(cmd, m)
				if result.quit {
					return m, m.chatQuitOrBack()
				}
				if result.output != "" {
					m.state.messages = append(m.state.messages, chatDisplayMessage{
						Role:    roleInfo,
						Content: result.output,
					})
				}
				m.refreshViewport()
				return m, nil
			}
		}
		return m.handleSendMessage()

	case key.Matches(msg, tui.ChatKeys.PageUp):
		var vpCmd tea.Cmd
		m.viewport, vpCmd = m.viewport.Update(msg)
		m.state.followMode = false
		return m, vpCmd

	case key.Matches(msg, tui.ChatKeys.PageDown):
		var vpCmd tea.Cmd
		m.viewport, vpCmd = m.viewport.Update(msg)
		return m, vpCmd

	case key.Matches(msg, tui.ChatKeys.Home):
		m.viewport.GotoTop()
		m.state.followMode = false
		return m, nil

	case key.Matches(msg, tui.ChatKeys.End):
		m.viewport.GotoBottom()
		m.state.followMode = true
		return m, nil
	}

	// Autocomplete navigation: intercept Up/Down/Tab before textarea
	if ac.active {
		switch msg.String() {
		case "up":
			ac.moveUp()
			return m, nil
		case "down":
			ac.moveDown()
			return m, nil
		case "tab":
			cmd := ac.selectedCommand()
			if cmd != "" {
				m.textarea.Reset()
				m.textarea.InsertString(cmd + " ")
				ac.reset()
				m.viewport.SetHeight(m.viewportHeight())
				m.resizeTextarea()
			}
			return m, nil
		}
	}

	// Input history navigation: Up/Down when not in autocomplete and not streaming
	if !ac.active && !m.state.active && len(m.state.inputHistory) > 0 {
		st := m.state
		switch msg.String() {
		case "up":
			if st.inputHistoryIdx > 0 {
				// Save current input when starting to navigate
				if st.inputHistoryIdx == len(st.inputHistory) {
					st.inputHistoryDraft = m.textarea.Value()
				}
				st.inputHistoryIdx--
				m.textarea.Reset()
				m.textarea.InsertString(st.inputHistory[st.inputHistoryIdx])
				m.resizeTextarea()
			}
			return m, nil
		case "down":
			if st.inputHistoryIdx < len(st.inputHistory) {
				st.inputHistoryIdx++
				m.textarea.Reset()
				if st.inputHistoryIdx == len(st.inputHistory) {
					// Restore draft
					m.textarea.InsertString(st.inputHistoryDraft)
				} else {
					m.textarea.InsertString(st.inputHistory[st.inputHistoryIdx])
				}
				m.resizeTextarea()
			}
			return m, nil
		}
	}

	// Forward all other keys to textarea
	var taCmd tea.Cmd
	m.textarea, taCmd = m.textarea.Update(msg)
	m.resizeTextarea()

	// Recompute autocomplete after textarea content changes
	prevHeight := ac.height()
	ac.update(m.textarea.Value())
	if ac.height() != prevHeight {
		m.viewport.SetHeight(m.viewportHeight())
		if m.state.followMode {
			m.viewport.GotoBottom()
		}
	}

	return m, taCmd
}

// handleSendMessage processes Enter key
func (m *ChatModel) handleSendMessage() (tea.Model, tea.Cmd) {
	input := strings.TrimSpace(m.textarea.Value())
	if input == "" {
		return m, nil
	}
	// A second Enter while the first response is still streaming would
	// orphan the in-flight goroutine (its ch + ctx get overwritten by
	// startStreamCmd), hold the backend concurrency slot, and wipe the
	// partial content buffer via resetStreaming. Require Ctrl+C first.
	if m.state.active {
		return m, nil
	}

	m.textarea.Reset()
	m.resizeTextarea()
	st := m.state
	st.autocomplete.reset()

	// Save to input history for arrow-up recall (capped to avoid unbounded growth)
	st.inputHistory = append(st.inputHistory, input)
	if len(st.inputHistory) > inputHistoryMax {
		st.inputHistory = st.inputHistory[len(st.inputHistory)-inputHistoryMax:]
	}
	st.inputHistoryIdx = len(st.inputHistory)
	st.inputHistoryDraft = ""

	// Slash commands
	if strings.HasPrefix(input, "/") {
		result := executeChatCommand(input, m)
		if result.quit {
			return m, m.chatQuitOrBack()
		}
		if result.output != "" {
			st.messages = append(st.messages, chatDisplayMessage{
				Role:    roleInfo,
				Content: result.output,
			})
		}
		m.refreshViewport()
		return m, nil
	}

	// Add user message
	st.messages = append(st.messages, chatDisplayMessage{
		Role:    roleUser,
		Content: input,
	})
	st.history = append(st.history, pkgClient.ChatMessage{
		Role:    roleUser,
		Content: input,
	})

	// Reset streaming state and display name (will be enriched when first chunk arrives)
	st.resetStreaming()
	st.animFrame = 0
	m.displayName = m.modelName

	// Add placeholder assistant message
	st.messages = append(st.messages, chatDisplayMessage{
		Role: roleAssistant,
	})

	m.refreshViewport()
	return m, tea.Batch(m.startStreamCmd(), ui.ShimmerTickCmd())
}

// startStreamCmd launches the SSE streaming in a goroutine
func (m *ChatModel) startStreamCmd() tea.Cmd {
	st := m.state
	ch := make(chan tea.Msg, 64)
	st.ch = ch

	ctx, cancel := context.WithCancel(context.Background()) //nolint:gosec // cancel stored on st.cancelStream and invoked by stopStream
	st.cancelStream = cancel

	go func() {
		defer close(ch)
		err := SendChatRequestStream(ctx, m.client, m.modelName, st.history,
			m.temperature, m.topP, m.maxTokens, m.PreferredNode,
			func(chunk pkgClient.StreamChunk) error {
				ch <- streamChunkMsg{chunk: chunk}
				return nil
			})
		if err != nil && ctx.Err() == nil {
			ch <- streamDoneMsg{err: err}
		} else {
			ch <- streamDoneMsg{}
		}
	}()

	return waitForStream(ch)
}

// waitForStream returns a tea.Cmd that reads the next message from the stream channel
func waitForStream(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return streamDoneMsg{}
		}
		return msg
	}
}

// handleStreamChunk processes a streaming token
func (m *ChatModel) handleStreamChunk(msg streamChunkMsg) (tea.Model, tea.Cmd) {
	m.processStreamChunk(msg.chunk)
	m.refreshViewport()

	if m.state.ch != nil {
		return m, waitForStream(m.state.ch)
	}
	return m, nil
}

// handleStreamDone processes stream completion
func (m *ChatModel) handleStreamDone(msg streamDoneMsg) (tea.Model, tea.Cmd) {
	st := m.state
	st.active = false
	st.ch = nil

	if msg.err != nil {
		st.messages = append(st.messages, chatDisplayMessage{
			Role:    roleInfo,
			Content: formatChatError(msg.err),
		})
		if len(st.history) > 0 && st.history[len(st.history)-1].Role == roleUser {
			st.history = st.history[:len(st.history)-1]
		}
	} else {
		m.finishAssistantMessage()
	}

	st.statusMessage = ""
	m.refreshViewport()
	return m, nil
}

// finishAssistantMessage finalizes the streaming message with stats
func (m *ChatModel) finishAssistantMessage() {
	st := m.state
	content := st.content.String()
	thinking := st.thinking.String()

	if len(st.messages) > 0 {
		idx := len(st.messages) - 1
		stats := m.computeStats(content, thinking)
		st.messages[idx].Content = content
		st.messages[idx].Thinking = thinking
		st.messages[idx].Stats = stats
		if stats != nil {
			st.totalMsgTokens += stats.TotalTokens
		}
		m.invalidateStreamingMessage()
	}

	if content != "" {
		msg := pkgClient.ChatMessage{
			Role:    roleAssistant,
			Content: content,
		}
		if thinking != "" {
			msg.Thinking = thinking
		}
		st.history = append(st.history, msg)
	}
}

// computeStats builds stats for a completed response
func (m *ChatModel) computeStats(content, thinking string) *chatMsgStats {
	st := m.state
	endTime := timeNow()
	totalDuration := endTime.Sub(st.startTime).Seconds()

	var prefillMs float64
	var decodeSec float64
	if st.firstTokenRecv {
		prefillMs = float64(st.firstTokenTime.Sub(st.startTime).Milliseconds())
		decodeSec = endTime.Sub(st.firstTokenTime).Seconds()
	} else {
		decodeSec = totalDuration
	}

	if st.totalTokens > 0 {
		decodeSpeed := float64(0)
		if decodeSec > 0 {
			decodeSpeed = float64(st.completionTokens) / decodeSec
		}
		// Prefill speed based on eval'd tokens (actual GPU work), not total prompt
		evalTokens := st.promptTokens - st.cachedTokens
		if evalTokens <= 0 {
			evalTokens = st.promptTokens
		}
		prefillSpeed := float64(0)
		if prefillMs > 0 && evalTokens > 0 {
			prefillSpeed = float64(evalTokens) / (prefillMs / 1000)
		}
		return &chatMsgStats{
			PrefillMs:        prefillMs,
			PrefillSpeed:     prefillSpeed,
			DecodeSeconds:    decodeSec,
			DecodeSpeed:      decodeSpeed,
			PromptTokens:     st.promptTokens,
			CompletionTokens: st.completionTokens,
			TotalTokens:      st.totalTokens,
			CachedTokens:     st.cachedTokens,
			ReasoningTokens:  st.reasoningTokens,
			Cost:             st.cost,
		}
	}

	// Estimate from content length
	promptChars := 0
	for _, msg := range st.history {
		promptChars += len(msg.Content)
		if msg.Thinking != "" {
			promptChars += len(msg.Thinking)
		}
	}
	estPrompt := promptChars / 4
	estCompletion := (len(content) + len(thinking)) / 4
	decodeSpeed := float64(0)
	if decodeSec > 0 {
		decodeSpeed = float64(estCompletion) / decodeSec
	}
	return &chatMsgStats{
		PrefillMs:        prefillMs,
		DecodeSeconds:    decodeSec,
		DecodeSpeed:      decodeSpeed,
		PromptTokens:     estPrompt,
		CompletionTokens: estCompletion,
		TotalTokens:      estPrompt + estCompletion,
		IsEstimate:       true,
	}
}

// refreshViewport rebuilds the viewport content and scrolls if needed
func (m *ChatModel) refreshViewport() {
	content := m.renderAllMessages()
	m.viewport.SetContent(content)
	if m.state.followMode {
		m.viewport.GotoBottom()
	}
}
