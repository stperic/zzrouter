package quota

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCalculateCostMicro_ProviderSource(t *testing.T) {
	micro, src := CalculateCostMicro(0.000001989, nil, "openrouter", []string{"gpt-4"}, 19, 3, 0, 0)
	assert.Equal(t, USDToMicro(0.000001989), micro)
	assert.Equal(t, CostSourceProvider, src)
}

func TestCalculateCostMicro_NoSource(t *testing.T) {
	micro, src := CalculateCostMicro(0, nil, "", []string{"unknown"}, 10, 5, 0, 0)
	assert.Equal(t, int64(0), micro)
	assert.Equal(t, "", src,
		"no provider cost AND no pricing store ⇒ empty source ⇒ omit from wire")
}
