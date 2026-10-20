package runsettings

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidKey(t *testing.T) {
	for key, valid := range map[string]bool{
		"future_flag":   true,
		"Future-flag2":  true,
		"bad key --x=1": false,
		"-lead":         false,
		"2lead":         false,
		"":              false,
	} {
		t.Run(key, func(t *testing.T) { assert.Equal(t, valid, ValidKey(key)) })
	}
}
