package install

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stretchr/testify/assert"
)

func TestCheckPlatformUsesDeclaredOSAndArchitecture(t *testing.T) {
	current := fsroot.Platform{OS: "linux", Arch: "amd64"}
	for _, tc := range []struct {
		name      string
		platforms []fsroot.Platform
		allowed   bool
	}{
		{"unconstrained", nil, true},
		{"matching", []fsroot.Platform{current}, true},
		{"any architecture", []fsroot.Platform{{OS: "linux"}}, true},
		{"wrong OS", []fsroot.Platform{{OS: "darwin", Arch: "amd64"}}, false},
		{"wrong architecture", []fsroot.Platform{{OS: "linux", Arch: "arm64"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckPlatform("test-runtime", tc.platforms, current)
			if tc.allowed {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, ErrUnsupportedPlatform)
				assert.ErrorContains(t, err, "test-runtime")
				assert.ErrorContains(t, err, current.String())
			}
		})
	}
}
