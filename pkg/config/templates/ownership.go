package templates

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stperic/zzrouter/pkg/utils"
	"gopkg.in/yaml.v3"
)

const ownershipFile = ".managed-ownership.json"
const maxOwnershipBytes = 1 << 20 // Bounds historical evidence, including unknown future keys.

type ownershipReceipt struct {
	Format int                   `json:"format"`
	Files  map[string][][]string `json:"files"`
}

func managedPaths(template map[string]any) [][]string {
	var paths [][]string
	for _, section := range managedKeySections(template) {
		for _, key := range section.keys {
			path := []string{key}
			if section.name != "" {
				path = []string{section.name, key}
			}
			paths = append(paths, path)
		}
	}
	return paths
}

func prepareManagedFiles(src fs.FS, dstDir string) (map[string][]byte, map[string][][]string, error) {
	files := map[string][]byte{}
	err := fs.WalkDir(src, providersRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, providersRoot+"/")
		if shippedFileOwnership(rel) != notManaged {
			files[rel], err = fs.ReadFile(src, p)
		}
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	history, err := managedHistory(files, dstDir)
	return files, history, err
}

func managedHistory(files map[string][]byte, dstDir string) (map[string][][]string, error) {
	if os.Geteuid() == 0 {
		return nil, nil // Root bootstrap must never inspect service-owned evidence.
	}
	receipt := ownershipReceipt{Format: 1, Files: map[string][][]string{}}
	path := filepath.Join(dstDir, ownershipFile)
	var previous []byte
	f, err := os.Open(path)
	if err == nil {
		receipt = ownershipReceipt{}
		previous, err = utils.ReadCapped(f, maxOwnershipBytes)
		_ = f.Close() // Windows refuses replacement while this handle is open.
		if err == nil {
			err = json.Unmarshal(previous, &receipt)
		}
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read ownership receipt: %w", err)
	}
	if receipt.Format != 1 || receipt.Files == nil {
		return nil, fmt.Errorf("invalid ownership receipt format")
	}
	for rel, data := range files {
		if shippedFileOwnership(rel) == managedKeys {
			var doc map[string]any
			if err := yaml.Unmarshal(data, &doc); err != nil {
				return nil, err
			}
			receipt.Files[rel] = append(receipt.Files[rel], managedPaths(doc)...)
		}
	}
	for rel, paths := range receipt.Files {
		if !fs.ValidPath(rel) || strings.ContainsAny(rel, "\\:\x00") || shippedFileOwnership(rel) != managedKeys {
			return nil, fmt.Errorf("invalid ownership file %q", rel)
		}
		for _, path := range paths {
			key := strings.Join(path, "/")
			if len(path) < 1 || len(path) > 2 || !fs.ValidPath(key) || strings.ContainsAny(key, "\\:\x00") || strings.Count(key, "/") != len(path)-1 {
				return nil, fmt.Errorf("invalid ownership path in %s", rel)
			}
		}
		slices.SortFunc(paths, slices.Compare[[]string])
		receipt.Files[rel] = slices.CompactFunc(paths, slices.Equal[[]string])
	}
	next, _ := json.Marshal(receipt)
	if len(next) > maxOwnershipBytes {
		return nil, utils.ErrResponseTooLarge
	}
	if !bytes.Equal(previous, next) {
		if err := utils.AtomicWriteFile(path, next, providerFileMode); err != nil {
			return nil, err
		}
	}
	return receipt.Files, archiveUnavailable(receipt.Files, files, dstDir)
}

func archiveUnavailable(history map[string][][]string, files map[string][]byte, dstDir string) error {
	for _, rel := range slices.Sorted(maps.Keys(history)) {
		live := filepath.Join(dstDir, filepath.FromSlash(rel))
		from, to := live, live+".rollback"
		if _, shipped := files[rel]; shipped {
			from, to = to, from
		}
		if _, err := os.Lstat(from); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := utils.RequireAbsent(to); err != nil {
			return fmt.Errorf("archive collision for %s: %w", rel, err)
		}
		if err := os.Rename(from, to); err != nil {
			return err
		}
		slog.Info("Moved provider configuration", "from", from, "to", to)
	}
	return nil
}
