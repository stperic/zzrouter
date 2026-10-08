package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Shell metacharacters that have no legitimate place in a provider's launch
// command or argument. zzRouter invokes providers via exec.Command (no
// shell), so these characters in provider config are either a tampered file
// or a misconfiguration — reject rather than execute.
//
// Parentheses are intentionally absent: they appear in legitimate Windows
// paths such as `C:\Program Files (x86)\...`.
const execShellMetaChars = ";|&$`<>{}[]\n\r"

// execAbsPathMetaChars is the stricter subset applied to absolute-path
// commands: control characters that have no legitimate place in any
// filesystem path on any supported OS.
const execAbsPathMetaChars = "\n\r\x00"

// execCommandNameRegex validates a bare command name (no path component).
var execCommandNameRegex = regexp.MustCompile(`^[a-zA-Z0-9._+-]+$`)

// execArgTemplateRegex matches the ${VAR} substitution tokens that
// CommandBuilder expands at launch (${PORT}, ${MODEL_PATH}, ...). Tokens
// must be closed — an unterminated `${...` falls through to the metachar
// scan.
var execArgTemplateRegex = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// ValidateExecutionCommand checks an ExecutionConfig's Command and Args for
// shell metacharacters and structural sanity. Defense in depth against a
// tampered provider config: zzRouter does not pass these through a shell, but
// their presence indicates either tampering or a misconfiguration and should
// fail loud at load time.
//
// Empty Command is allowed (template entries awaiting configurator fill).
// ${VAR} substitution tokens in Args are recognized and excluded from the
// metachar scan.
func ValidateExecutionCommand(providerName string, exec ExecutionConfig) error {
	if exec.Command != "" {
		if err := validateExecCommandField(providerName, exec.Command); err != nil {
			return err
		}
	}
	for i, arg := range exec.Args {
		if err := validateExecArgField(providerName, i, arg); err != nil {
			return err
		}
	}
	return validateExecutionPlaceholders(providerName, exec)
}

func validateExecCommandField(providerName, cmd string) error {
	if filepath.IsAbs(cmd) {
		if strings.ContainsAny(cmd, execAbsPathMetaChars) {
			return fmt.Errorf("provider %q: execution.command %q contains control character", providerName, cmd)
		}
		return nil
	}
	if strings.ContainsAny(cmd, execShellMetaChars) {
		return fmt.Errorf("provider %q: execution.command %q contains shell metacharacter", providerName, cmd)
	}
	if !execCommandNameRegex.MatchString(cmd) {
		return fmt.Errorf("provider %q: execution.command %q must be an absolute path or a simple name (alphanumeric, dots, hyphens, underscores, plus)", providerName, cmd)
	}
	return nil
}

func validateExecArgField(providerName string, idx int, arg string) error {
	stripped := execArgTemplateRegex.ReplaceAllString(arg, "")
	if strings.ContainsAny(stripped, execShellMetaChars) {
		return fmt.Errorf("provider %q: execution.args[%d] %q contains shell metacharacter", providerName, idx, arg)
	}
	return nil
}
