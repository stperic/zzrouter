// Package config provides OS-agnostic configuration file management for zzRouter
// Cross-platform editor launching utilities
package config

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/stperic/zzrouter/pkg/host"
)

// GetPreferredEditor returns the user's preferred editor
// Checks in priority order:
// 1. ZZROUTER_EDITOR environment variable (explicit override)
// 2. VISUAL environment variable (standard Unix)
// 3. EDITOR environment variable (standard Unix)
// 4. Config file editor (if specified AND available)
// 5. Auto-detected system editors (nano, vim, notepad, etc.)
func GetPreferredEditor() string {
	// Priority 1: ZZROUTER-specific editor (explicit override)
	if editor := os.Getenv("ZZROUTER_EDITOR"); editor != "" {
		return editor
	}

	// Priority 2: VISUAL (preferred for full-screen editors)
	if editor := os.Getenv("VISUAL"); editor != "" {
		return editor
	}

	// Priority 3: EDITOR (standard fallback)
	if editor := os.Getenv("EDITOR"); editor != "" {
		return editor
	}

	// Priority 4: Config file editor (if specified AND available)
	if editor := getEditorFromConfig(); editor != "" {
		// Validate that the configured editor is actually available
		if isEditorAvailable(editor) {
			return editor
		}
		// If configured but not available, fall through to auto-detection
	}

	// Priority 5: Auto-detect available system editors
	return getDefaultEditor()
}

// getEditorFromConfig attempts to load editor from client config
func getEditorFromConfig() string {
	cm := NewConfigManager("zzrouter")

	// Try to load client config
	clientConfig, err := cm.LoadClientConfig()
	if err != nil {
		return "" // Config not available, skip
	}

	return clientConfig.Preferences.Editor
}

// isEditorAvailable checks if the specified editor is available
func isEditorAvailable(editor string) bool {
	// Special case for macOS "open" commands
	if editor == "open -e" || editor == "open -a TextEdit" {
		return isCommandAvailable("open")
	}

	return isCommandAvailable(editor)
}

// getDefaultEditor returns the default editor for the current platform
func getDefaultEditor() string {
	switch runtime.GOOS {
	case "darwin": // macOS
		// Check if common editors are available
		if isCommandAvailable("nano") {
			return "nano" // User-friendly, always available
		}
		if isCommandAvailable("vim") {
			return "vim"
		}
		return "open -e" // TextEdit as last resort

	case "linux":
		// Check common Linux editors
		if isCommandAvailable("nano") {
			return "nano" // Most user-friendly
		}
		if isCommandAvailable("vim") {
			return "vim"
		}
		if isCommandAvailable("vi") {
			return "vi" // Always available on Unix
		}
		return "nano" // Fallback

	case "windows":
		// Windows defaults
		if isCommandAvailable("notepad.exe") {
			return "notepad.exe"
		}
		return "notepad" // Should always be available

	default:
		return "vi" // Unix fallback
	}
}

// isCommandAvailable checks if a command is available in PATH
func isCommandAvailable(command string) bool {
	_, err := exec.LookPath(command)
	return err == nil
}

// OpenInEditor opens a file in the user's preferred editor
func OpenInEditor(filepath string) error {
	editor := GetPreferredEditor()

	// Special handling for macOS "open -e" command
	if editor == "open -e" {
		cmd := host.Command("open", "-e", filepath)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	// For all other editors
	cmd := host.Command(editor, filepath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// OpenInEditorWithFallback opens a file in editor with user-friendly error messages
func OpenInEditorWithFallback(filepath string) error {
	editor := GetPreferredEditor()

	// Check if editor is available before trying to open
	if err := ValidateEditor(); err != nil {
		// Editor not found - provide helpful error with alternatives
		availableEditors := findAvailableEditors()

		var errorMsg strings.Builder
		errorMsg.WriteString(fmt.Sprintf("Editor '%s' is not installed or not in PATH.\n\n", editor))

		if len(availableEditors) > 0 {
			errorMsg.WriteString("Available editors on your system:\n")
			for _, ed := range availableEditors {
				errorMsg.WriteString(fmt.Sprintf("  - %s\n", ed))
			}
			errorMsg.WriteString("\nTo use one of these editors:\n")
			errorMsg.WriteString(fmt.Sprintf("  export EDITOR=%s\n", availableEditors[0]))
			errorMsg.WriteString("  export ZZROUTER_EDITOR=nano\n")
		} else {
			errorMsg.WriteString("No common editors found. Please install one:\n")
			errorMsg.WriteString("  - nano (user-friendly, terminal)\n")
			errorMsg.WriteString("  - vim (powerful, terminal)\n")
			errorMsg.WriteString("  - code (VS Code)\n")
		}

		errorMsg.WriteString(fmt.Sprintf("\nOr edit the file manually:\n  %s", filepath))

		return fmt.Errorf("%s", errorMsg.String())
	}

	fmt.Printf("Opening %s in %s...\n", filepath, editor)

	err := OpenInEditor(filepath)
	if err != nil {
		return fmt.Errorf("failed to open editor '%s': %w\n\nYou can edit the file manually:\n  %s", editor, err, filepath)
	}

	return nil
}

// findAvailableEditors returns a list of editors that are actually installed
func findAvailableEditors() []string {
	suggestions := SuggestEditors()
	available := []string{}

	for _, editor := range suggestions {
		// Handle special cases like "open -e"
		if editor == "open -e" || editor == "open -a TextEdit" {
			if isCommandAvailable("open") {
				available = append(available, editor)
			}
			continue
		}

		if isCommandAvailable(editor) {
			available = append(available, editor)
		}
	}

	return available
}

// SuggestEditors returns a list of recommended editors for the platform
func SuggestEditors() []string {
	switch runtime.GOOS {
	case "darwin": // macOS
		return []string{
			"nano",             // User-friendly, terminal
			"vim",              // Powerful, terminal
			"code",             // VS Code
			"subl",             // Sublime Text
			"atom",             // Atom
			"open -e",          // TextEdit (GUI)
			"open -a TextEdit", // TextEdit (explicit)
		}
	case "linux":
		return []string{
			"nano",  // User-friendly, terminal
			"vim",   // Powerful, terminal
			"vi",    // Classic, always available
			"emacs", // Powerful, terminal/GUI
			"code",  // VS Code
			"gedit", // GNOME Text Editor
			"kate",  // KDE Text Editor
		}
	case "windows":
		return []string{
			"notepad.exe",   // Simple, always available
			"notepad++.exe", // Notepad++
			"code.cmd",      // VS Code
			"subl.exe",      // Sublime Text
		}
	default:
		return []string{"nano", "vim", "vi"}
	}
}

// ValidateEditor checks if the configured editor is available
func ValidateEditor() error {
	editor := GetPreferredEditor()

	// Special case for macOS "open" commands
	if editor == "open -e" || editor == "open -a TextEdit" {
		if !isCommandAvailable("open") {
			return fmt.Errorf("'open' command not found (macOS only)")
		}
		return nil
	}

	// Check if editor command exists
	if !isCommandAvailable(editor) {
		return fmt.Errorf("editor '%s' not found in PATH", editor)
	}

	return nil
}
