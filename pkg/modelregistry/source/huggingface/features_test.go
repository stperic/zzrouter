package huggingface

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigVisionEvidence(t *testing.T) {
	for _, tc := range []struct {
		body          string
		known, vision bool
	}{{`{"vision_config":{}}`, true, true}, {`{"vision_config":null}`, true, false}, {`{}`, false, false}, {`invalid`, false, false}} {
		dir := t.TempDir()
		file := filepath.Join(dir, "config.json")
		require.NoError(t, os.WriteFile(file, []byte(tc.body), 0600))
		extra := parseHuggingFaceConfig(file)
		details, _ := extra["details"].(map[string]any)
		vision, known := details["vision"].(bool)
		assert.Equal(t, tc.known, known)
		assert.Equal(t, tc.vision, vision)
	}
}
