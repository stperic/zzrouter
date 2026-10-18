package schema

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/stperic/zzrouter/pkg/config/assets"
	"github.com/stperic/zzrouter/pkg/config/runsettings"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Check validates one parameter in the canonical string form a launch passes
// against shapes, the rules a PATCH and a launch share: the name is declared,
// the value has the declared type and lies within its range, enum or asset
// name rules. A shape that declares Auto takes the auto keyword.
func Check(key, value string, shapes map[string]ParamShape) *utils.ParamError {
	shape, known := shapes[key]
	if !known {
		message := fmt.Sprintf("unknown parameter %q", key)
		if positive, negated := strings.CutPrefix(key, "no-"); negated && shapes[positive].Negatable {
			message += fmt.Sprintf("; set %s: false, which passes --%s", positive, key)
		}
		return &utils.ParamError{Key: key, Code: string(httperr.CodeUnknownFlag), Message: message}
	}
	if shape.Auto != "" && value == runsettings.Auto {
		return nil
	}
	if len(shape.Enum) > 0 && !slices.Contains(shape.Enum, value) {
		return &utils.ParamError{Key: key, Code: string(httperr.CodeCoercionFailed), Message: fmt.Sprintf("%s=%q not in allowed set %v", key, value, shape.Enum)}
	}
	switch shape.Kind {
	case ParamInt:
		n, err := strconv.ParseInt(value, 10, 64)
		if errors.Is(err, strconv.ErrRange) {
			return &utils.ParamError{Key: key, Code: string(httperr.CodeOutOfRange), Got: value, Message: fmt.Sprintf("%s=%s is outside the integer range", key, value)}
		}
		if err != nil {
			return WrongType(key, shape.Kind)
		}
		return checkRange(key, float64(n), shape)
	case ParamFloat:
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
			return WrongType(key, shape.Kind)
		}
		return checkRange(key, n, shape)
	case ParamBool:
		if value != "true" && value != "false" {
			return WrongType(key, shape.Kind)
		}
	case ParamString, "":
		if key == runsettings.KeepAliveParameter {
			if _, err := runsettings.Duration(value); err != nil {
				return &utils.ParamError{Key: key, Code: string(httperr.CodeCoercionFailed), Message: err.Error()}
			}
		}
	case ParamAsset:
		// Existence is checked where the name resolves: the PATCH apply and the launching node.
		if err := assets.ValidateName(value); err != nil {
			return &utils.ParamError{Key: key, Code: string(httperr.CodeInvalidValue), Want: "asset name", Message: fmt.Sprintf("%s: %v", key, err)}
		}
	default:
		// The kinds are a closed set; one this switch does not know would
		// otherwise pass any value unchecked.
		return &utils.ParamError{Key: key, Code: string(httperr.CodeWrongType), Message: fmt.Sprintf("%s has schema type %q, which this server cannot validate", key, shape.Kind)}
	}
	return nil
}

// Duplicate refuses spellings of one flag that one map gives different
// values, one issue per spelling so each can be removed by its own name.
func Duplicate(spellings []string) []utils.ParamError {
	message := fmt.Sprintf("%s spell one flag with different values", strings.Join(spellings, " and "))
	issues := make([]utils.ParamError, len(spellings))
	for i, key := range spellings {
		issues[i] = utils.ParamError{Key: key, Code: string(httperr.CodeDuplicateFlag), Message: message}
	}
	return issues
}

// WrongType refuses a value that is not of kind, in the words a caller of
// either PATCH or a launch reads.
func WrongType(key string, kind ParamKind) *utils.ParamError {
	want, message := "string", "%s must be a string"
	switch kind {
	case ParamInt:
		want, message = "int", "%s must be an integer"
	case ParamFloat:
		want, message = "float", "%s must be a number"
	case ParamBool:
		want, message = "bool", "%s must be a boolean"
	case ParamAsset:
		message = "%s must be an asset name"
	}
	return &utils.ParamError{Key: key, Code: string(httperr.CodeWrongType), Want: want, Message: fmt.Sprintf(message, key)}
}

func checkRange(key string, n float64, shape ParamShape) *utils.ParamError {
	if shape.Min != nil && n < *shape.Min {
		return &utils.ParamError{Key: key, Code: string(httperr.CodeOutOfRange), Got: n, Min: *shape.Min, Message: fmt.Sprintf("%s=%v below minimum %v", key, n, *shape.Min)}
	}
	if shape.Max != nil && n > *shape.Max {
		return &utils.ParamError{Key: key, Code: string(httperr.CodeOutOfRange), Got: n, Max: *shape.Max, Message: fmt.Sprintf("%s=%v exceeds maximum %v", key, n, *shape.Max)}
	}
	return nil
}
