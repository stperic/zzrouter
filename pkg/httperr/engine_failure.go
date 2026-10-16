package httperr

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/stperic/zzrouter/pkg/utils"
)

// FailureClass is the closed set of engine failures zzRouter recognizes. A
// provider declares which of its engine's messages belong to which class.
type FailureClass string

const (
	FailureMemory                FailureClass = "memory"
	FailureUnsupportedInput      FailureClass = "unsupported_input"
	FailureChatTemplateRejected  FailureClass = "chat_template_rejected"
	FailureToolCallingNotEnabled FailureClass = "tool_calling_not_enabled"
)

// Error codes for the classes that keep their class name as their code.
const (
	CodeUnsupportedInput      = string(FailureUnsupportedInput)
	CodeChatTemplateRejected  = string(FailureChatTemplateRejected)
	CodeToolCallingNotEnabled = string(FailureToolCallingNotEnabled)
)

// Remedies the caller is told; shared by surfaces that report the class without a table lookup.
const (
	ChatTemplateRemedy = "An admin can upload a chat template that accepts it and select it for this model; GET /zzrouter/v1 explains how under chat_templates."
	ToolCallingRemedy  = `For vLLM, PATCH /zzrouter/v1/providers/{name}/parameters with {"models":{"{model}":{"parameters":{"enable-auto-tool-choice":true,"tool-call-parser":"{parser}"}}}} using the operator's per-family parser from the model publisher's serving instructions, then restart the run and probe again with zz_chat tools: true.`
)

// failureClasses is the one table: each class's status, code, message prefix and remedy.
var failureClasses = map[FailureClass]struct {
	status               int
	code, prefix, remedy string
}{
	FailureMemory:                {http.StatusConflict, string(CodeOutOfRange), "", "Release memory or reduce the configured budget before retrying."},
	FailureUnsupportedInput:      {http.StatusBadRequest, CodeUnsupportedInput, "", "Use a model that supports this input, or remove it from the request."},
	FailureChatTemplateRejected:  {http.StatusBadRequest, CodeChatTemplateRejected, "the model's chat template rejected this request: ", ChatTemplateRemedy},
	FailureToolCallingNotEnabled: {http.StatusBadRequest, CodeToolCallingNotEnabled, "this model's engine was launched without tool calling: ", ToolCallingRemedy},
}

// FailureClasses lists the class names, sorted, for config schema validation.
func FailureClasses() []string {
	classes := make([]string, 0, len(failureClasses))
	for class := range failureClasses {
		classes = append(classes, string(class))
	}
	slices.Sort(classes)
	return classes
}

// EngineFailure is a classified engine failure; it travels as a typed cause.
type EngineFailure struct {
	Class   FailureClass
	Message string
}

func (e *EngineFailure) Error() string { return e.Message }

// HTTPError renders the failure with its class's status, code and remedy.
func (e *EngineFailure) HTTPError() Error {
	spec := failureClasses[e.Class]
	message := spec.prefix + e.Message
	if spec.remedy != "" {
		message += " " + spec.remedy
	}
	return Error{Status: spec.status, Type: "invalid_request_error", Code: spec.code, Message: utils.SanitizeErrorMessage(message)}
}

// FailurePattern is one engine message a provider declares, and its class. A
// regex's first capture group, when present, is the reason reported.
type FailurePattern struct {
	Pattern  string       `yaml:"pattern" json:"pattern"`
	IsRegex  bool         `yaml:"isRegex" json:"isRegex"`
	Class    FailureClass `yaml:"class" json:"class"`
	compiled *regexp.Regexp
}

// Classifier is a provider's declared failure patterns. The zero value
// classifies nothing.
type Classifier []FailurePattern

// Compile validates every class and compiles every regex; Match skips an
// uncompiled regex.
func (c *Classifier) Compile() error {
	compiled := slices.Clone(*c)
	for i := range compiled {
		p := &compiled[i]
		if _, ok := failureClasses[p.Class]; !ok {
			return fmt.Errorf("unknown engine failure class %q", p.Class)
		}
		if strings.TrimSpace(p.Pattern) == "" {
			return fmt.Errorf("engine failure pattern must not be empty")
		}
		if p.IsRegex {
			re, err := regexp.Compile(p.Pattern)
			if err != nil {
				return fmt.Errorf("engine failure pattern %q: %w", p.Pattern, err)
			}
			p.compiled = re
		}
	}
	*c = compiled
	return nil
}

// maxFailureReason bounds engine details while leaving room for the remedy.
const maxFailureReason = 300

// Match returns the first declared pattern that matches message, or nil.
func (c *Classifier) Match(message string) *EngineFailure {
	return c.match(message, nil)
}

func (c *Classifier) match(message string, accept func(FailureClass) bool) *EngineFailure {
	if c == nil {
		return nil
	}
	for _, p := range *c {
		reason := message
		if p.IsRegex {
			if p.compiled == nil {
				continue
			}
			match := p.compiled.FindStringSubmatch(message)
			if match == nil {
				continue
			}
			if len(match) > 1 {
				reason = match[1]
			}
		} else if !strings.Contains(strings.ToLower(message), strings.ToLower(p.Pattern)) {
			continue
		}
		if accept != nil && !accept(p.Class) {
			continue
		}
		reason = strings.TrimSpace(reason)
		if reason == "" {
			reason = "(no reason given)."
		}
		if r := []rune(reason); len(r) > maxFailureReason {
			reason = string(r[:maxFailureReason]) + "..."
		}
		return &EngineFailure{Class: p.Class, Message: reason}
	}
	return nil
}

// classForCode maps a code a serving node already assigned back to its class.
func classForCode(code string) (FailureClass, bool) {
	for class, spec := range failureClasses {
		if spec.code == code {
			return class, true
		}
	}
	return "", false
}
