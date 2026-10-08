package quota

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// SpendFile is the on-disk format for spend.json.
type SpendFile struct {
	Version   int                    `json:"version"`
	UpdatedAt time.Time              `json:"updated_at"`
	States    map[string]*SpendState `json:"states"`
}

// LoadSpend reads spend state from disk. Returns empty state on corruption (startup resilience).
func LoadSpend(path string) (*SpendFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var file SpendFile
	if err := json.Unmarshal(data, &file); err != nil {
		slog.Warn("Corrupt spend.json, resetting to zero", "error", err, "path", path)
		return &SpendFile{Version: 1, States: make(map[string]*SpendState)}, nil
	}

	if file.States == nil {
		file.States = make(map[string]*SpendState)
	}

	return &file, nil
}

// SaveSpend writes spend state to disk using atomic temp-file + rename.
func SaveSpend(path string, file *SpendFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create spend dir: %w", err)
	}

	file.UpdatedAt = utils.NowUTC()

	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal spend state: %w", err)
	}

	return utils.AtomicWriteFile(path, data, 0o600)
}
