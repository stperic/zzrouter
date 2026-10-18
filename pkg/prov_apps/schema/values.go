package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
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
// is integral, which becomes an integer literal so 1e6 reaches an engine as
// 1000000. ok is false for null, objects and arrays.
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
		return integerLiteral(decoded.String()), true
	}
	return "", false
}

// literalBound caps the length and exponent of a literal integerLiteral
// rewrites through big.Rat: the integer it writes has as many digits as the
// exponent, and parsing grows with the mantissa (a 1 MiB literal takes about
// half a second). No engine's integer needs more; a number past the bound
// stays as written, which Check reads as out of range on an int.
const literalBound = 80

// integerLiteral writes an integral number literal as an integer literal and
// keeps any other as written.
func integerLiteral(literal string) string {
	if n, err := strconv.ParseInt(literal, 10, 64); err == nil {
		return strconv.FormatInt(n, 10) // -0 becomes 0, as -0.0 does below
	}
	if !strings.ContainsAny(literal, ".eE") || len(literal) > literalBound {
		return literal
	}
	if _, exponent, found := strings.Cut(strings.ToLower(literal), "e"); found {
		if e, err := strconv.Atoi(exponent); err != nil || e > literalBound || e < -literalBound {
			return literal
		}
	}
	if n, parsed := new(big.Rat).SetString(literal); parsed && n.IsInt() {
		return n.Num().String()
	}
	return literal
}
