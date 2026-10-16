package gpu

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsUnifiedMemory(t *testing.T) {
	for _, typ := range []string{"apple", "nvidia", "amd", "intel", "other", "none", ""} {
		assert.Equal(t, typ == "apple", IsUnifiedMemory(typ), typ)
	}
}
