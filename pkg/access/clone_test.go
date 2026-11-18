package access_test

import (
	"reflect"
	"testing"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/teams"
)

// A reference field added to a record later must be deep-copied by Clone too.
func TestCloneCopiesEveryReference(t *testing.T) {
	t.Parallel()
	for name, record := range map[string]any{"VirtualKey": &keys.VirtualKey{}, "Team": &teams.Team{}} {
		original := reflect.ValueOf(record)
		fillReferences(original.Elem())
		clone := original.MethodByName("Clone").Call(nil)[0]
		requireUnshared(t, name, original.Elem(), clone.Elem())
	}
}

func fillReferences(v reflect.Value) {
	for i := range v.NumField() {
		field := v.Field(i)
		if !v.Type().Field(i).IsExported() {
			continue
		}
		switch field.Kind() {
		case reflect.Pointer:
			field.Set(reflect.New(field.Type().Elem()))
		case reflect.Slice:
			field.Set(reflect.MakeSlice(field.Type(), 1, 1))
		case reflect.Map:
			entries := reflect.MakeMap(field.Type())
			entries.SetMapIndex(reflect.Zero(field.Type().Key()), reflect.Zero(field.Type().Elem()))
			field.Set(entries)
		case reflect.Struct:
			fillReferences(field)
		}
	}
}

func requireUnshared(t *testing.T, path string, original, clone reflect.Value) {
	t.Helper()
	for i := range original.NumField() {
		if !original.Type().Field(i).IsExported() {
			continue
		}
		name := path + "." + original.Type().Field(i).Name
		switch a, b := original.Field(i), clone.Field(i); a.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Map:
			if b.IsNil() || a.Pointer() == b.Pointer() {
				t.Errorf("%s is shared with or dropped by Clone", name)
			}
		case reflect.Struct:
			requireUnshared(t, name, a, b)
		}
	}
}
