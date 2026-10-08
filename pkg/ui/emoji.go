package ui

import (
	"os"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/term"
)

// Emoji support is detected once and cached for the process lifetime.
// Set once at startup via SetEmojiPreference before any goroutines start.
// The mutex serializes the lazy cache in SupportsEmoji against the
// preference-change path in SetEmojiPreference so concurrent readers
// (log lines from parallel goroutines) don't race on the cache flag.
var (
	emojiMu          sync.Mutex
	emojiPreference  string
	preferenceLoader func() string
	preferenceLoaded bool
	emojiCached      bool
	emojiResult      bool
)

// SetPreferenceLoader installs a deferred preference loader. The loader
// is called from SupportsEmoji on first use, not at binary start, so
// `--help`, `version`, completion, and any non-rendering code path
// avoid the disk I/O of reading the client config YAML.
//
// The loader is invoked while SupportsEmoji holds emojiMu, so it MUST
// NOT re-enter SupportsEmoji / GetEmoji / any ui function that takes
// emojiMu. Typical loaders (a config-file read) are fine; adding log
// lines that render emoji inside the loader would self-deadlock.
func SetPreferenceLoader(loader func() string) {
	emojiMu.Lock()
	defer emojiMu.Unlock()
	preferenceLoader = loader
	preferenceLoaded = false
	emojiCached = false
}

// SupportsEmoji checks if the terminal supports emoji rendering.
// The result is cached after the first call.
func SupportsEmoji() bool {
	emojiMu.Lock()
	defer emojiMu.Unlock()
	if emojiCached {
		return emojiResult
	}
	if !preferenceLoaded && preferenceLoader != nil {
		emojiPreference = strings.ToLower(strings.TrimSpace(preferenceLoader()))
		preferenceLoaded = true
		preferenceLoader = nil
	}
	emojiResult = detectEmojiSupport()
	emojiCached = true
	return emojiResult
}

func detectEmojiSupport() bool {
	switch emojiPreference {
	case "on", "true", "yes", "1":
		return true
	case "off", "false", "no", "0":
		return false
	}

	fd := int(os.Stdout.Fd())
	if !term.IsTerminal(fd) {
		return false
	}

	if os.Getenv("ZZROUTER_NO_EMOJI") == "1" || os.Getenv("NO_EMOJI") == "1" {
		return false
	}
	if os.Getenv("ZZROUTER_EMOJI") == "1" || os.Getenv("EMOJI") == "1" {
		return true
	}

	switch runtime.GOOS {
	case "windows":
		return supportsEmojiWindows()
	case "darwin", "linux":
		return supportsEmojiUnix()
	default:
		return false
	}
}

// supportsEmojiWindows detects emoji support on Windows
func supportsEmojiWindows() bool {
	// Windows Terminal (modern, supports emojis)
	if os.Getenv("WT_SESSION") != "" {
		return true
	}

	// ConEmu (may support emojis depending on version)
	if os.Getenv("ConEmuANSI") == "ON" {
		// ConEmu with ANSI support likely supports emojis
		return true
	}

	// Check TERM variable
	term := strings.ToLower(os.Getenv("TERM"))
	if term != "" && term != "dumb" {
		// If TERM is set to something other than "dumb", it's likely a modern terminal
		// Check for known emoji-supporting terminals
		if strings.Contains(term, "xterm") || strings.Contains(term, "256color") {
			return true
		}
	}

	// PowerShell 7+ (pwsh) typically supports emojis
	// Check if running in PowerShell by checking PSModulePath or other indicators
	if os.Getenv("PSModulePath") != "" {
		// PowerShell detected - check version if possible
		// PowerShell 7+ (Core) supports emojis, older versions may not
		// For now, assume PowerShell 7+ if PSModulePath exists
		return true
	}

	// Legacy Windows CMD and older PowerShell - likely no emoji support
	// Check for ANSICON (old ANSI support tool)
	if os.Getenv("ANSICON") != "" {
		// ANSICON may have limited emoji support, but it's unreliable
		return false
	}

	// Default: assume no emoji support for legacy Windows terminals
	return false
}

// supportsEmojiUnix detects emoji support on Unix-like systems (macOS, Linux)
func supportsEmojiUnix() bool {
	// Check TERM variable
	term := strings.ToLower(os.Getenv("TERM"))
	if term == "dumb" {
		return false
	}

	// Check COLORTERM for modern terminals
	colorterm := strings.ToLower(os.Getenv("COLORTERM"))
	if colorterm == "truecolor" || colorterm == "24bit" {
		// True color terminals typically support emojis
		return true
	}

	// Check for known emoji-supporting terminals
	if term != "" {
		// Modern terminals that support emojis
		emojiSupportingTerms := []string{
			"xterm-256color",
			"xterm-kitty",
			"alacritty",
			"wezterm",
			"iterm",
			"iterm2",
			"screen-256color",
			"tmux-256color",
		}
		for _, supported := range emojiSupportingTerms {
			if strings.Contains(term, supported) {
				return true
			}
		}
	}

	// Check for specific terminal emulators via environment variables
	if os.Getenv("KITTY_WINDOW_ID") != "" {
		// Kitty terminal supports emojis
		return true
	}
	if os.Getenv("ALACRITTY_LOG") != "" {
		// Alacritty terminal supports emojis
		return true
	}
	if os.Getenv("ITERM_SESSION_ID") != "" {
		// iTerm2 supports emojis
		return true
	}

	// macOS Terminal.app typically supports emojis (if TERM is set)
	if runtime.GOOS == "darwin" && term != "" {
		// macOS Terminal.app usually has emoji support
		return true
	}

	// Linux: check if we're in a modern terminal
	// Most modern Linux terminals support emojis if they support Unicode
	if term != "" && !strings.Contains(term, "linux") {
		// Not a Linux console (which doesn't support emojis well)
		// Assume modern terminal with emoji support
		return true
	}

	// Default: conservative approach - assume no emoji support
	return false
}

// GetEmoji returns the emoji if emoji support is detected, otherwise returns a fallback text
func GetEmoji(emoji, fallback string) string {
	if SupportsEmoji() {
		return emoji
	}
	return fallback
}

// providerIcon holds emoji and ASCII fallback for a provider.
type providerIcon struct {
	Emoji    string
	Fallback string
}

// providerIcons is the single source of truth for all provider icons.
var providerIcons = map[string]providerIcon{
	// Local providers
	"ollama":       {"\U0001F999", "[O]"},   // 🦙
	"huggingface":  {"\U0001F917", "[HF]"},  // 🤗
	"vllm":         {"\u26A1", "[VL]"},      // ⚡
	"llamacpp":     {"\U0001F527", "[LC]"},  // 🔧
	"llama.cpp":    {"\U0001F527", "[LC]"},  // 🔧
	"mlx":          {"\U0001F34E", "[MLX]"}, // 🍎
	"tensorrt":     {"\U0001F680", "[TR]"},  // 🚀
	"tensorrt-llm": {"\U0001F680", "[TR]"},  // 🚀
	// Cloud providers
	"anthropic":  {"\U0001F300", "[AN]"},   // 🌀
	"openai":     {"\U0001F916", "[OA]"},   // 🤖
	"openrouter": {"\U0001F500", "[OR]"},   // 🔀
	"groq":       {"\u26A1", "[GQ]"},       // ⚡
	"gemini":     {"\U0001F48E", "[GM]"},   // 💎
	"cloudflare": {"\u2601\uFE0F", "[CF]"}, // ☁️
	"bedrock":    {"\U0001F4E6", "[BR]"},   // 📦
	"azure":      {"\U0001F30A", "[AZ]"},   // 🌊
}

var defaultIcon = providerIcon{"\U0001F310", "[?]"} // 🌐

// GetProviderEmoji returns a terminal-appropriate emoji or ASCII fallback for a provider.
// This is the single source of truth for all provider icons across the codebase.
func GetProviderEmoji(provider string) string {
	icon := lookupProviderIcon(provider)
	return GetEmoji(icon.Emoji, icon.Fallback)
}

// GetProviderEmojiRaw returns the raw unicode emoji for a provider (ignoring terminal capability).
// Used by server API responses that send the icon string to clients.
func GetProviderEmojiRaw(provider string) string {
	return lookupProviderIcon(provider).Emoji
}

func lookupProviderIcon(provider string) providerIcon {
	if icon, ok := providerIcons[strings.ToLower(provider)]; ok {
		return icon
	}
	return defaultIcon
}

// GetSuccessEmoji returns success emoji or fallback text
func GetSuccessEmoji() string {
	return GetEmoji("✅", "[OK]")
}

// GetErrorEmoji returns error emoji or fallback text
func GetErrorEmoji() string {
	return GetEmoji("❌", "[ERROR]")
}

// GetWarningEmoji returns warning emoji or fallback text
func GetWarningEmoji() string {
	return GetEmoji("⚠️", "[WARN]")
}

// GetInfoEmoji returns info emoji or fallback text
func GetInfoEmoji() string {
	return GetEmoji("💡", "[INFO]")
}

// GetSearchEmoji returns search emoji or fallback text
func GetSearchEmoji() string {
	return GetEmoji("🔍", "[SEARCH]")
}

// GetCheckEmoji returns checkmark emoji or fallback text
func GetCheckEmoji() string {
	return GetEmoji("✓", "[OK]")
}

// GetFolderEmoji returns folder emoji or fallback text
func GetFolderEmoji() string {
	return GetEmoji("📁", "[DIR]")
}
