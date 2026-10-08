// Package server provides type-specific validation functions.

package server

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// validateString validates a string parameter.
func validateString(rule ParamRule, value string, result *ValidationResult) *ValidationError {
	// Length validation
	if rule.MinLength > 0 && len(value) < rule.MinLength {
		return &ValidationError{
			Parameter: rule.Name,
			Message:   fmt.Sprintf("must be at least %d characters", rule.MinLength),
		}
	}

	if rule.MaxLength > 0 && len(value) > rule.MaxLength {
		return &ValidationError{
			Parameter: rule.Name,
			Message:   fmt.Sprintf("must be at most %d characters", rule.MaxLength),
		}
	}

	// Allowed values validation
	if len(rule.AllowedValues) > 0 {
		allowed := slices.Contains(rule.AllowedValues, value)
		if !allowed {
			return &ValidationError{
				Parameter: rule.Name,
				Message:   fmt.Sprintf("must be one of: %s", strings.Join(rule.AllowedValues, ", ")),
			}
		}
	}

	result.strings[rule.Name] = value
	return nil
}

// validateInt validates and converts an integer parameter.
func validateInt(rule ParamRule, value string, result *ValidationResult) *ValidationError {
	intVal, err := strconv.Atoi(value)
	if err != nil {
		return &ValidationError{
			Parameter: rule.Name,
			Message:   "must be a valid integer",
		}
	}

	// Range validation
	if rule.MinValue != nil && intVal < *rule.MinValue {
		return &ValidationError{
			Parameter: rule.Name,
			Message:   fmt.Sprintf("must be at least %d", *rule.MinValue),
		}
	}

	if rule.MaxValue != nil && intVal > *rule.MaxValue {
		return &ValidationError{
			Parameter: rule.Name,
			Message:   fmt.Sprintf("must be at most %d", *rule.MaxValue),
		}
	}

	result.ints[rule.Name] = intVal
	return nil
}

// validateFloat validates and converts a float parameter.
func validateFloat(rule ParamRule, value string, result *ValidationResult) *ValidationError {
	floatVal, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return &ValidationError{
			Parameter: rule.Name,
			Message:   "must be a valid number",
		}
	}

	// Range validation (using int min/max for simplicity)
	if rule.MinValue != nil && floatVal < float64(*rule.MinValue) {
		return &ValidationError{
			Parameter: rule.Name,
			Message:   fmt.Sprintf("must be at least %d", *rule.MinValue),
		}
	}

	if rule.MaxValue != nil && floatVal > float64(*rule.MaxValue) {
		return &ValidationError{
			Parameter: rule.Name,
			Message:   fmt.Sprintf("must be at most %d", *rule.MaxValue),
		}
	}

	result.floats[rule.Name] = floatVal
	return nil
}

// validateBool validates and converts a boolean parameter.
func validateBool(rule ParamRule, value string, result *ValidationResult) *ValidationError {
	switch strings.ToLower(value) {
	case "true", "1", "yes":
		result.bools[rule.Name] = true
	case "false", "0", "no", "":
		result.bools[rule.Name] = false
	default:
		return &ValidationError{
			Parameter: rule.Name,
			Message:   "must be a boolean (true/false, 1/0, yes/no)",
		}
	}

	return nil
}
