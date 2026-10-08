package upstream

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRecipeSpecifierComparisons(t *testing.T) {
	for _, tc := range []struct {
		version, constraint string
		allowed             bool
	}{
		{"2.10.0+cu130", "==2.10.0", true}, {"2.10.0+cu130", "==2.10.0+cu128", false},
		{"1.0+ABC_001", "==1.0+abc.1", true}, {"1.0+abc.1", "!=1.0+abc_01", false},
		{"1.02.3", "==1.2.*", true}, {"1.2", "==1.2.0.*", true}, {"1.20", "==1.2.*", false},
		{"1!2.1.3", "~=1!2.1", true}, {"2!2.1.3", "~=1!2.1", false},
		{"1.4.6", "~=1.4.5rc1", true}, {"1.5.0", "~=1.4.5rc1", false},
		{"1.0rc1", "<1.0", false}, {"1.0.post1", ">1.0", false},
		{"1.0+cu130", ">1.0", false}, {"1.0.post2", ">1.0.post1", true},
		{"1.0rc2", ">=1.0rc1,<1.0rc3", true}, {"3.0", ">=2,<3", false},
	} {
		assert.Equal(t, tc.allowed, VersionAllowed(tc.version, tc.constraint) == nil, "%s %s", tc.version, tc.constraint)
	}
}
