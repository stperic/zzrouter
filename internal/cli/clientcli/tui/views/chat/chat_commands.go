package chat

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/atotto/clipboard"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// chatCommandResult holds the result of a slash command execution
type chatCommandResult struct {
	output string // Text to display as info message
	quit   bool   // True if the user wants to exit
}

// executeChatCommand processes a slash command and returns the result
func executeChatCommand(input string, m *ChatModel) chatCommandResult {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return chatCommandResult{}
	}

	command := strings.ToLower(parts[0])

	switch command {
	case "/verbose":
		m.state.showStats = !m.state.showStats
		saveChatSettings(m)
		if m.state.showStats {
			return chatCommandResult{output: "Verbose mode enabled"}
		}
		return chatCommandResult{output: "Verbose mode disabled"}

	case "/thinking":
		m.state.showThinking = !m.state.showThinking
		saveChatSettings(m)
		if m.state.showThinking {
			return chatCommandResult{output: "Thinking display enabled"}
		}
		return chatCommandResult{output: "Thinking display disabled"}

	case "/temperature", "/temp":
		return chatTemperatureCommand(parts, m)

	case "/top_p":
		return chatTopPCommand(parts, m)

	case "/max_tokens":
		return chatMaxTokensCommand(parts, m)

	case "/copy":
		return chatCopyCommand(m)

	case "/clear":
		m.state.history = []pkgClient.ChatMessage{}
		m.state.messages = []chatDisplayMessage{{
			Role:    roleInfo,
			Content: "Session context cleared.",
		}}
		m.state.renderCache = make(map[int]string)
		return chatCommandResult{}

	case "/system":
		if len(parts) < 2 {
			for _, msg := range m.state.history {
				if msg.Role == roleSystem {
					return chatCommandResult{output: "System: " + msg.Content}
				}
			}
			return chatCommandResult{output: "No system prompt set. Usage: /system <message>"}
		}
		systemMsg := strings.Join(parts[1:], " ")
		if len(m.state.history) > 0 && m.state.history[0].Role == roleSystem {
			m.state.history[0].Content = systemMsg
		} else {
			m.state.history = append([]pkgClient.ChatMessage{{Role: roleSystem, Content: systemMsg}}, m.state.history...)
		}
		saveChatSettings(m)
		return chatCommandResult{output: "System prompt set: " + systemMsg}

	case "/bye", "/exit", "/quit":
		return chatCommandResult{output: "Goodbye!", quit: true}

	case "/?", "/help":
		return chatCommandResult{output: chatHelp()}

	default:
		return chatCommandResult{output: fmt.Sprintf("Unknown command: %s: Type /? for help", command)}
	}
}

func chatTemperatureCommand(parts []string, m *ChatModel) chatCommandResult {
	if len(parts) < 2 {
		if m.temperature < 0 {
			return chatCommandResult{output: "Temperature: default (not sent to provider)"}
		}
		return chatCommandResult{output: fmt.Sprintf("Temperature: %.1f", m.temperature)}
	}
	if parts[1] == "default" || parts[1] == "off" || parts[1] == "-1" {
		m.temperature = -1
		saveChatSettings(m)
		return chatCommandResult{output: "Temperature reset to default (not sent to provider)"}
	}
	val, err := strconv.ParseFloat(parts[1], 64)
	if err != nil || val < 0 || val > 2 {
		return chatCommandResult{output: "Invalid value. Usage: /temperature <0.0-2.0> or /temperature default"}
	}
	m.temperature = val
	saveChatSettings(m)
	return chatCommandResult{output: fmt.Sprintf("Temperature set to %.1f", val)}
}

func chatTopPCommand(parts []string, m *ChatModel) chatCommandResult {
	if len(parts) < 2 {
		if m.topP < 0 {
			return chatCommandResult{output: "Top-P: not set (provider default)"}
		}
		return chatCommandResult{output: fmt.Sprintf("Top-P: %.2f", m.topP)}
	}
	val, err := strconv.ParseFloat(parts[1], 64)
	if err != nil || val < 0 || val > 1 {
		return chatCommandResult{output: "Invalid value. Usage: /top_p <0.0-1.0>"}
	}
	m.topP = val
	saveChatSettings(m)
	return chatCommandResult{output: fmt.Sprintf("Top-P set to %.2f", val)}
}

func chatMaxTokensCommand(parts []string, m *ChatModel) chatCommandResult {
	if len(parts) < 2 {
		if m.maxTokens == 0 {
			return chatCommandResult{output: "Max tokens: unlimited"}
		}
		return chatCommandResult{output: fmt.Sprintf("Max tokens: %d", m.maxTokens)}
	}
	val, err := strconv.Atoi(parts[1])
	if err != nil || val < 0 {
		return chatCommandResult{output: "Invalid value. Usage: /max_tokens <n> (0 = unlimited)"}
	}
	m.maxTokens = val
	saveChatSettings(m)
	if val == 0 {
		return chatCommandResult{output: "Max tokens set to unlimited"}
	}
	return chatCommandResult{output: fmt.Sprintf("Max tokens set to %d", val)}
}

// chatCopyCommand copies the last assistant response to clipboard
func chatCopyCommand(m *ChatModel) chatCommandResult {
	var lastContent string
	for i := len(m.state.messages) - 1; i >= 0; i-- {
		if m.state.messages[i].Role == roleAssistant && m.state.messages[i].Content != "" {
			lastContent = m.state.messages[i].Content
			break
		}
	}
	if lastContent == "" {
		return chatCommandResult{output: "No assistant response to copy"}
	}

	if err := clipboard.WriteAll(lastContent); err != nil {
		return chatCommandResult{output: fmt.Sprintf("Copy failed: %v", err)}
	}

	preview := lastContent
	if len(preview) > 60 {
		preview = preview[:60] + "..."
	}
	preview = strings.ReplaceAll(preview, "\n", " ")
	return chatCommandResult{output: fmt.Sprintf("Copied to clipboard: %s", preview)}
}

func chatHelp() string {
	return `Commands:
  /verbose            Toggle verbose mode (show LLM stats)
  /thinking           Toggle thinking/reasoning display
  /temperature <n>    Set temperature (0.0-2.0, -1 = model default)
  /top_p <n>          Set top-p sampling (0.0-1.0, -1 = model default)
  /max_tokens <n>     Set max response tokens (0 = unlimited)
  /copy               Copy last response to clipboard
  /clear              Clear session context
  /system <msg>       Set system prompt
  /bye                Exit

Keyboard Shortcuts:
  Enter               Send message
  Shift+Enter         New line in message
  Alt+C               Copy last response
  Ctrl+C              Cancel stream / Back
  Ctrl+D              Back to menu
  PgUp/PgDn           Scroll message history
  Home/End            Jump to top/bottom`
}

// saveChatSettings persists current chat settings to cli.yaml
func saveChatSettings(m *ChatModel) {
	cm := pkgConfig.NewConfigManager("zzrouter")
	store, err := pkgConfig.NewClientConfigStore(cm)
	if err != nil {
		return
	}

	var systemPrompt string
	for _, msg := range m.state.history {
		if msg.Role == roleSystem {
			systemPrompt = msg.Content
			break
		}
	}

	_ = store.SetChatSettings(pkgConfig.ChatSettings{
		Verbose:      m.state.showStats,
		Thinking:     m.state.showThinking,
		Temperature:  m.temperature,
		TopP:         m.topP,
		MaxTokens:    m.maxTokens,
		SystemPrompt: systemPrompt,
	})
}
