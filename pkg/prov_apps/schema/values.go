package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
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

// UnmarshalJSON keeps each value in its Canonical form.
func (p *ParameterValues) UnmarshalJSON(data []byte) error {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	var params ParameterValues
	if values != nil {
		params = make(ParameterValues, len(values))
	}
	for key, raw := range values {
		value, ok := Canonical(raw)
		if !ok {
			return &ParameterValueError{Key: key}
		}
		params[key] = value
	}
	*p = params
	return nil
}

// Canonical is the string a JSON scalar becomes as a parameter value: a
// string as itself, a boolean as its literal, a number as written unless it
// is integral and fits an int64, which becomes an integer literal so 1e6
// reaches an engine as 1000000. ok is false for null, objects and arrays.
func Canonical(raw json.RawMessage) (value string, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded any
	if dec.Decode(&decoded) != nil {
		return "", false
	}
	switch decoded := decoded.(type) {
	case string:
		return decoded, true
	case bool:
		return strconv.FormatBool(decoded), true
	case json.Number:
		if n, parsed := new(big.Rat).SetString(decoded.String()); parsed && n.IsInt() && n.Num().IsInt64() {
			return n.Num().String(), true
		}
		return decoded.String(), true
	}
	return "", false
}
