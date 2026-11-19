package sqlitedb

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"strings"
)

// Schema identifies a database owner and its immutable, numbered SQL migrations with SHA256 SUMS.
type Schema struct {
	// AppID is the application_id assigned to newly created files.
	AppID int32
	// FS holds NNNN_name.sql migrations numbered from 0001 without gaps and their SUMS file.
	FS fs.FS
}

func (s Schema) migrations() ([]string, error) {
	entries, err := fs.ReadDir(s.FS, ".")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		if len(entry.Name()) < 10 || entry.Name()[:5] != fmt.Sprintf("%04d_", len(names)+1) {
			return nil, fmt.Errorf("migration %s: expected number %04d", entry.Name(), len(names)+1)
		}
		names = append(names, entry.Name())
	}
	return names, nil
}

// Latest returns the last migration number, rejecting gaps and duplicate numbers.
func (s Schema) Latest() (int, error) {
	names, err := s.migrations()
	return len(names), err
}

// Admit refuses a header from another application (ErrForeign), a newer
// schema (ErrNewer) or an invalid version (ErrUnreadable).
func (s Schema) Admit(h Header) error {
	if h.AppID != s.AppID {
		return fmt.Errorf("%w: got %d, want %d", ErrForeign, h.AppID, s.AppID)
	}
	latest, err := s.Latest()
	if err != nil {
		return err
	}
	if h.Version > latest {
		return fmt.Errorf("%w: got %d, latest %d", ErrNewer, h.Version, latest)
	}
	if h.Version < 0 {
		return fmt.Errorf("%w: invalid schema version: %d", ErrUnreadable, h.Version)
	}
	return nil
}

// Verify checks migration numbering and exact SUMS coverage so released migrations cannot change silently.
func (s Schema) Verify() error {
	names, err := s.migrations()
	if err != nil {
		return err
	}
	data, err := fs.ReadFile(s.FS, "SUMS")
	if err != nil {
		return err
	}
	sums := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || sums[fields[1]] != "" {
			return fmt.Errorf("invalid SUMS entry: %q", line)
		}
		sums[fields[1]] = fields[0]
	}
	for _, name := range names {
		data, err := fs.ReadFile(s.FS, name)
		if err != nil {
			return err
		}
		if sums[name] != fmt.Sprintf("%x", sha256.Sum256(data)) {
			return fmt.Errorf("missing or mismatched checksum: %s", name)
		}
		delete(sums, name)
	}
	if len(sums) != 0 {
		return fmt.Errorf("SUMS contains missing migrations: %v", sums)
	}
	return nil
}
