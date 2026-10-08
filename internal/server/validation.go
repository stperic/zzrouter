// Package server provides declarative validation for HTTP request parameters.
// This eliminates manual validation boilerplate across handlers.

package server

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
)

// ParamRule defines a validation rule for a request parameter.
type ParamRule struct {
	// Name is the parameter name (from path, query, or header)
	Name string

	// Source indicates where to extract the parameter from
	Source ParamSource

	// Type indicates the parameter type (string, int, float, bool)
	Type ParamType

	// Required indicates if the parameter is mandatory
	Required bool

	// Default is the default value if not provided (as string)
	Default string

	// ErrorMessage is a custom error message (optional)
	ErrorMessage string

	// MinLength for string parameters (0 = no check)
	MinLength int

	// MaxLength for string parameters (0 = no check)
	MaxLength int

	// MinValue for numeric parameters (nil = no check)
	MinValue *int

	// MaxValue for numeric parameters (nil = no check)
	MaxValue *int

	// AllowedValues restricts parameter to specific values (nil = no restriction)
	AllowedValues []string
}

// ParamSource indicates where a parameter comes from.
type ParamSource int

const (
	// ParamSourcePath extracts from URL path (/api/:param)
	ParamSourcePath ParamSource = iota

	// ParamSourceQuery extracts from query string (?param=value)
	ParamSourceQuery

	// ParamSourceHeader extracts from HTTP headers
	ParamSourceHeader

	// ParamSourceBody extracts from request body (JSON)
	ParamSourceBody
)

// ParamType indicates the parameter type.
type ParamType int

const (
	// ParamTypeString is a string parameter (default)
	ParamTypeString ParamType = iota

	// ParamTypeInt is an integer parameter
	ParamTypeInt

	// ParamTypeFloat is a float parameter
	ParamTypeFloat

	// ParamTypeBool is a boolean parameter
	ParamTypeBool
)

// ValidationError represents a parameter validation failure.
type ValidationError struct {
	Parameter string
	Message   string
}

// Error implements the error interface.
func (e *ValidationError) Error() string {
	return fmt.Sprintf("parameter '%s': %s", e.Parameter, e.Message)
}

// ValidationResult holds validated and typed parameter values.
type ValidationResult struct {
	strings map[string]string
	ints    map[string]int
	floats  map[string]float64
	bools   map[string]bool
}

// GetString returns a string parameter value.
func (r *ValidationResult) GetString(name string) string {
	return r.strings[name]
}

// GetInt returns an integer parameter value.
func (r *ValidationResult) GetInt(name string) int {
	return r.ints[name]
}

// GetBool returns a boolean parameter value.
func (r *ValidationResult) GetBool(name string) bool {
	return r.bools[name]
}

// ValidateParams validates request parameters according to the given rules.
// Returns ValidationResult with typed values, or sends error response and returns nil.
//
// Usage:
//
//	rules := []ParamRule{
//	    RequiredPathParam("model_name"),
//	    IntQueryParam("limit", 20, 1, 100),
//	}
//	params := ValidateParams(c, rules)
//	if params == nil {
//	    return // error response already sent
//	}
//	modelName := params.GetString("model_name")
//	limit := params.GetInt("limit")
func ValidateParams(c *gin.Context, rules []ParamRule) *ValidationResult {
	result := &ValidationResult{
		strings: make(map[string]string),
		ints:    make(map[string]int),
		floats:  make(map[string]float64),
		bools:   make(map[string]bool),
	}
	var validationErrors []ValidationError

	for _, rule := range rules {
		if err := extractAndValidateTyped(c, rule, result); err != nil {
			validationErrors = append(validationErrors, *err)
		}
	}

	if len(validationErrors) > 0 {
		sendValidationError(c, validationErrors)
		return nil
	}

	return result
}

// extractAndValidateTyped extracts and validates a parameter, storing it in the typed result.
func extractAndValidateTyped(c *gin.Context, rule ParamRule, result *ValidationResult) *ValidationError {
	// Extract raw value based on source
	var rawValue string
	switch rule.Source {
	case ParamSourcePath:
		rawValue = c.Param(rule.Name)
	case ParamSourceQuery:
		rawValue = c.Query(rule.Name)
	case ParamSourceHeader:
		rawValue = c.GetHeader(rule.Name)
	default:
		return &ValidationError{
			Parameter: rule.Name,
			Message:   "unsupported parameter source",
		}
	}

	// Apply default if value is empty
	if rawValue == "" && rule.Default != "" {
		rawValue = rule.Default
	}

	// Required check
	if rule.Required && rawValue == "" {
		msg := rule.ErrorMessage
		if msg == "" {
			msg = "is required"
		}
		return &ValidationError{
			Parameter: rule.Name,
			Message:   msg,
		}
	}

	// If not required and empty, return early
	if rawValue == "" {
		return nil
	}

	// Type-specific validation and conversion
	switch rule.Type {
	case ParamTypeInt:
		return validateInt(rule, rawValue, result)
	case ParamTypeFloat:
		return validateFloat(rule, rawValue, result)
	case ParamTypeBool:
		return validateBool(rule, rawValue, result)
	default: // ParamTypeString
		return validateString(rule, rawValue, result)
	}
}

// sendValidationError sends a standardized validation error response.
func sendValidationError(c *gin.Context, errors []ValidationError) {
	errorMessages := make([]string, len(errors))
	for i, err := range errors {
		errorMessages[i] = err.Error()
	}

	BadRequest(c, "validation failed: "+strings.Join(errorMessages, "; "))
}

// Common validation rule builders for reuse across handlers

// RequiredPathParam creates a rule for a required path parameter.
func RequiredPathParam(name string) ParamRule {
	return ParamRule{
		Name:         name,
		Source:       ParamSourcePath,
		Type:         ParamTypeString,
		Required:     true,
		MinLength:    1,
		ErrorMessage: "is required and must not be empty",
	}
}

// OptionalQueryParam creates a rule for an optional query parameter.
func OptionalQueryParam(name string) ParamRule {
	return ParamRule{
		Name:     name,
		Source:   ParamSourceQuery,
		Type:     ParamTypeString,
		Required: false,
	}
}

// OptionalQueryParamWithDefault creates an optional query parameter with a default value.
func OptionalQueryParamWithDefault(name, defaultValue string) ParamRule {
	return ParamRule{
		Name:     name,
		Source:   ParamSourceQuery,
		Type:     ParamTypeString,
		Required: false,
		Default:  defaultValue,
	}
}

// RequiredQueryParam creates a rule for a required query parameter.
func RequiredQueryParam(name string) ParamRule {
	return ParamRule{
		Name:      name,
		Source:    ParamSourceQuery,
		Type:      ParamTypeString,
		Required:  true,
		MinLength: 1,
	}
}

// RequiredEnumQueryParam creates a rule for a required query parameter
// constrained to a fixed set of values. The validation error lists all
// allowed values so AI agents can parse them without a separate schema
// lookup (e.g. validateString emits "must be one of: models, providers,
// popular" when a wrong value is given).
func RequiredEnumQueryParam(name string, allowed []string) ParamRule {
	return ParamRule{
		Name:          name,
		Source:        ParamSourceQuery,
		Type:          ParamTypeString,
		Required:      true,
		MinLength:     1,
		AllowedValues: allowed,
	}
}

// BoolQueryParam creates a rule for a boolean query parameter.
func BoolQueryParam(name string) ParamRule {
	return ParamRule{
		Name:     name,
		Source:   ParamSourceQuery,
		Type:     ParamTypeBool,
		Required: false,
		Default:  "false",
	}
}

// IntQueryParam creates a rule for an integer query parameter with min/max range.
func IntQueryParam(name string, defaultValue, minValue, maxValue int) ParamRule {
	min := minValue
	max := maxValue
	return ParamRule{
		Name:     name,
		Source:   ParamSourceQuery,
		Type:     ParamTypeInt,
		Required: false,
		Default:  fmt.Sprintf("%d", defaultValue),
		MinValue: &min,
		MaxValue: &max,
	}
}
