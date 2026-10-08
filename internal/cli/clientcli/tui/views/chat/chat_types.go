package chat

import (
	"context"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"

	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Chat TUI constants
const (
	chatMaxTextWidth  = 100 // Cap glamour render width
	chatInputMinLines = 1   // Textarea default height (scrolls internally)
	inputHistoryMax   = 200 // Max entries kept for arrow-up recall
)

// Message role constants
const (
	roleUser      = "user"
	roleAssistant = "assistant"
	roleSystem    = "system"
	roleInfo      = "info"
)

// hermesParseState tracks the Hermes 3 tag parsing state machine
type hermesParseState int

const (
	hermesNormal hermesParseState = iota
	hermesInThinking
	hermesInFinal
)

// chatDisplayMessage holds a message for display rendering
type chatDisplayMessage struct {
	Role     string // roleUser, roleAssistant, roleSystem, roleInfo
	Content  string
	Thinking string
	Stats    *chatMsgStats
}

// chatMsgStats holds performance statistics for a completed assistant response
type chatMsgStats struct {
	PrefillMs        float64
	PrefillSpeed     float64
	DecodeSeconds    float64
	DecodeSpeed      float64
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CachedTokens     int     // Prompt tokens served from KV cache
	ReasoningTokens  int     // Tokens used for reasoning/thinking
	Cost             float64 // Cost in USD (from cloud providers)
	IsEstimate       bool
}

// chatState holds all mutable state behind a pointer so it survives
// Bubble Tea's value-copy of the model in Update().
type chatState struct {
	// Chat messages
	messages []chatDisplayMessage
	history  []pkgClient.ChatMessage

	// Generation parameters (mutable via /set commands)
	showThinking bool
	showStats    bool

	// Streaming
	active           bool
	cancelStream     context.CancelFunc // cancels the in-flight HTTP request
	ch               <-chan tea.Msg
	content          strings.Builder
	thinking         strings.Builder
	buffer           strings.Builder
	hermesState      hermesParseState
	thinkingStarted  bool
	startTime        time.Time
	firstTokenTime   time.Time
	firstTokenRecv   bool
	promptTokens     int
	completionTokens int
	totalTokens      int
	cachedTokens     int
	reasoningTokens  int
	cost             float64

	// Status
	statusMessage string
	modelLoaded   bool // true after first successful response

	// Render cache
	renderCache map[int]string

	// Glamour renderer
	glamourRenderer *glamour.TermRenderer
	glamourWidth    int

	// Follow mode
	followMode bool

	// Message counter for stats display
	totalMsgTokens int

	// Animation frame for shimmer/spinner during generation
	animFrame int

	// Footer layout
	footerLines int

	// Slash command autocomplete
	autocomplete autocompleteState

	// Input history (arrow up/down to recall previous inputs)
	inputHistory      []string
	inputHistoryIdx   int    // current position; len(inputHistory) means "new input"
	inputHistoryDraft string // saved draft when user starts navigating
}

// resetStreaming resets all streaming-related fields before a new request.
func (st *chatState) resetStreaming() {
	st.active = true
	st.content.Reset()
	st.thinking.Reset()
	st.buffer.Reset()
	st.hermesState = hermesNormal
	st.thinkingStarted = false
	st.startTime = utils.Now()
	st.firstTokenRecv = false
	st.promptTokens = 0
	st.completionTokens = 0
	st.totalTokens = 0
	st.cachedTokens = 0
	st.reasoningTokens = 0
	st.cost = 0
	st.statusMessage = ""
}

// ChatModel is the Bubble Tea model for the chat TUI.
type ChatModel struct {
	// Connection (immutable after init)
	client      *pkgClient.Client
	modelName   string // Original model name sent to the API (never mutated)
	displayName string // Model name shown in the UI (enriched with provider/node info)
	nodeAddress string // e.g. "localhost:9090"
	temperature float64
	topP        float64
	maxTokens   int

	// Theme styles (immutable after init)
	styles ui.Styles

	// Embedded mode: when true, exit actions return tui.NavBack() instead of tea.Quit
	Embedded bool

	// All mutable state behind a pointer
	state *chatState

	// UI components
	viewport viewport.Model
	textarea textarea.Model
	ready    bool

	// Layout
	termWidth  int
	termHeight int

	// Node count for title bar (set by parent TUI)
	NodeCount int

	// Preferred node for inference routing (X-Node header)
	PreferredNode string
}

// Tea messages
type (
	streamChunkMsg struct {
		chunk pkgClient.StreamChunk
	}
	streamDoneMsg struct {
		err error
	}
)

// chatQuitOrBack returns a tea.Cmd that either quits (standalone) or goes back
// to the parent TUI menu (embedded mode).
func (m *ChatModel) chatQuitOrBack() tea.Cmd {
	if m.Embedded {
		return tui.NavBack()
	}
	return tea.Quit
}

// Cancel implements tui.Canceller. Root invokes this when the chat view
// leaves the stack so any in-flight stream goroutine shuts down.
func (m *ChatModel) Cancel() {
	if m.state.cancelStream != nil {
		m.state.cancelStream()
		m.state.cancelStream = nil
	}
}

// newChatModel creates an initialized ChatModel
func NewChatModel(client *pkgClient.Client, modelName, nodeAddress, systemPrompt string,
	temperature, topP float64, maxTokens int, showThinking bool, styles ui.Styles) *ChatModel {

	ta := textarea.New()
	ta.Placeholder = "Send a message... (Type / for commands, Enter to send, Shift+Enter for a new line)"
	ta.Focus()
	ta.CharLimit = 0
	ta.SetHeight(chatInputMinLines)
	ta.ShowLineNumbers = false
	ta.KeyMap.InsertNewline.SetKeys("shift+enter")
	// Style the textarea
	s := ta.Styles()
	clear := lipgloss.NewStyle()
	s.Focused.Base = clear
	s.Focused.Text = clear
	s.Focused.CursorLine = clear
	s.Focused.Prompt = clear
	s.Focused.EndOfBuffer = clear
	s.Focused.Placeholder = lipgloss.NewStyle().Foreground(styles.Theme.Overlay)
	s.Blurred = s.Focused
	ta.SetStyles(s)

	var history []pkgClient.ChatMessage
	var displayMsgs []chatDisplayMessage

	if systemPrompt != "" {
		history = append(history, pkgClient.ChatMessage{
			Role:    roleSystem,
			Content: systemPrompt,
		})
		displayMsgs = append(displayMsgs, chatDisplayMessage{
			Role:    roleInfo,
			Content: "System: " + systemPrompt,
		})
	}

	// Welcome hint is shown in the textarea placeholder instead of as a message

	return &ChatModel{
		client:      client,
		modelName:   modelName,
		displayName: modelName,
		nodeAddress: nodeAddress,
		temperature: temperature,
		topP:        topP,
		maxTokens:   maxTokens,
		styles:      styles,
		textarea:    ta,
		state: &chatState{
			messages:     displayMsgs,
			history:      history,
			showThinking: showThinking,
			renderCache:  make(map[int]string),
			followMode:   true,
		},
	}
}

// Init implements tea.Model
func (m *ChatModel) Init() tea.Cmd {
	return m.textarea.Focus()
}

// Compile-time interface checks for ChatModel.
var (
	_ tea.Model     = (*ChatModel)(nil)
	_ tui.Canceller = (*ChatModel)(nil)
)

// SetShowStats sets the verbose stats display flag.
func (m *ChatModel) SetShowStats(v bool) { m.state.showStats = v }
