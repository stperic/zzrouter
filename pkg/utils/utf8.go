package utils

import (
	"reflect"
	"unicode/utf8"
)

// ValidUTF8 reports whether every string in v, map keys included, is valid
// UTF-8. JSON replaces any other byte sequence, so a value stored as JSON must
// pass it to read back unchanged.
func ValidUTF8(v any) bool { return validUTF8(reflect.ValueOf(v)) }

func validUTF8(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.String:
		return utf8.ValidString(v.String())
	case reflect.Interface, reflect.Pointer:
		return v.IsNil() || validUTF8(v.Elem())
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if !validUTF8(v.Index(i)) {
				return false
			}
		}
	case reflect.Map:
		for entry := v.MapRange(); entry.Next(); {
			if !validUTF8(entry.Key()) || !validUTF8(entry.Value()) {
				return false
			}
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if !validUTF8(v.Field(i)) {
				return false
			}
		}
	}
	return true
}
