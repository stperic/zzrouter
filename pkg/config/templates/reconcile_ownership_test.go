package templates

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestMergeManagedSpineSectionOwnership(t *testing.T) {
	for _, section := range []struct{ name, managed, operator string }{
		{"runtime", "execution", "base_port"},
		{"capabilities", "wire_endpoints", "priority"},
	} {
		t.Run(section.name, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				present bool
				value   any
				want    any
				changed bool
			}{
				{name: "absent", want: "previous"},
				{name: "null", present: true, want: "previous"},
				{name: "scalar", present: true, value: "invalid", want: "previous"},
				{name: "sequence", present: true, value: []any{}, want: "previous"},
				{name: "empty map", present: true, value: map[string]any{}, changed: true},
				{name: "managed value", present: true, value: map[string]any{section.managed: "release"}, want: "release", changed: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					live, err := yaml.Marshal(map[string]any{
						"enabled":    true,
						section.name: map[string]any{section.managed: "previous", section.operator: 17},
					})
					require.NoError(t, err)
					template := map[string]any{}
					if tc.present {
						template[section.name] = tc.value
					}
					tmpl, err := yaml.Marshal(template)
					require.NoError(t, err)
					got, changed, err := mergeManagedSpine(live, tmpl)
					require.NoError(t, err)
					assert.Equal(t, tc.changed, changed)
					var doc map[string]any
					require.NoError(t, yaml.Unmarshal(got, &doc))
					body, ok := doc[section.name].(map[string]any)
					require.True(t, ok)
					if tc.want == nil {
						assert.NotContains(t, body, section.managed)
					} else {
						assert.Equal(t, tc.want, body[section.managed])
					}
					assert.Equal(t, 17, body[section.operator])
					assert.Equal(t, true, doc["enabled"])
					again, changed, err := mergeManagedSpine(got, tmpl)
					require.NoError(t, err)
					assert.False(t, changed)
					assert.Equal(t, got, again)
				})
			}
		})
	}
}

func TestMergeManagedSpinePinOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, template string
		want           any
		changed        bool
	}{
		{"unseeded", "{}\n", "operator", false},
		{"seeded", "pinned_version: release\n", "release", true},
		{"explicit null", "pinned_version: null\n", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, changed, err := mergeManagedSpine([]byte("pinned_version: operator\n"), []byte(tc.template))
			require.NoError(t, err)
			assert.Equal(t, tc.changed, changed)
			var doc map[string]any
			require.NoError(t, yaml.Unmarshal(got, &doc))
			assert.Contains(t, doc, "pinned_version")
			assert.Equal(t, tc.want, doc["pinned_version"])
			again, changed, err := mergeManagedSpine(got, []byte(tc.template))
			require.NoError(t, err)
			assert.False(t, changed)
			assert.Equal(t, got, again)
		})
	}
}

func TestMergeManagedSpineDeletesRetiredRootKey(t *testing.T) {
	got, changed, err := mergeManagedSpine([]byte("enabled: true\nmodel_defaults: {}\n"), []byte("{}\n"))
	require.NoError(t, err)
	assert.True(t, changed)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(got, &doc))
	assert.NotContains(t, doc, "model_defaults")
	assert.Equal(t, true, doc["enabled"])
}
