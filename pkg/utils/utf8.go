package utils

import (
	"reflect"
	"unicode/utf8"
)

// ValidUTF8 reports whether every string in v, map keys included, is valid
// UTF-8. JSON replaces any other byte sequence, so a value stored as JSON must
// pass it to read back unchanged.
func ValidUTF8(v any) bool { return validUTF8(reflect.ValueOf(v), make(map[visit]bool)) }

// visit is a pointer, map or slice already being checked; a cycle ends there.
type visit struct {
	ptr uintptr
	typ reflect.Type
	len int
}

func validUTF8(v reflect.Value, seen map[visit]bool) bool {
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice:
		if v.IsNil() {
			return true
		}
		at := visit{v.Pointer(), v.Type(), 0}
		if v.Kind() == reflect.Slice {
			at.len = v.Len()
		}
		if seen[at] {
			return true
		}
		seen[at] = true
	}
	switch v.Kind() {
	case reflect.String:
		return utf8.ValidString(v.String())
	case reflect.Interface, reflect.Pointer:
		return v.IsNil() || validUTF8(v.Elem(), seen)
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if !validUTF8(v.Index(i), seen) {
				return false
			}
		}
	case reflect.Map:
		for entry := v.MapRange(); entry.Next(); {
			if !validUTF8(entry.Key(), seen) || !validUTF8(entry.Value(), seen) {
				return false
			}
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if !validUTF8(v.Field(i), seen) {
				return false
			}
		}
	}
	return true
}
