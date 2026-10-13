package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// ParameterValues accepts JSON scalars at request boundaries and keeps canonical strings.
type ParameterValues map[string]string

// ParameterValueError identifies a parameter whose JSON value is not a supported scalar.
type ParameterValueError struct {
	Key string
}

func (e *ParameterValueError) Error() string {
	return fmt.Sprintf("parameters.%s must be a JSON string, number or boolean; null, objects and arrays are not allowed", e.Key)
}

// UnmarshalJSON preserves number literals without passing them through floating point.
func (p *ParameterValues) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var values map[string]any
	if err := dec.Decode(&values); err != nil {
		return err
	}
	var params ParameterValues
	if values != nil {
		params = make(ParameterValues, len(values))
	}
	for key, value := range values {
		switch value := value.(type) {
		case string:
			params[key] = value
		case json.Number:
			params[key] = value.String()
		case bool:
			params[key] = strconv.FormatBool(value)
		default:
			return &ParameterValueError{Key: key}
		}
	}
	*p = params
	return nil
}
