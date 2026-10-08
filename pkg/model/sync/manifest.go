// Package sync handles node-to-node model transfer over the mTLS cluster
// port. It owns the wire-level SyncManifest format, per-file SHA-256
// verification, path-traversal security, and atomic streaming downloads.
//
// Naming note: this package shares its short name with the standard
// library's `sync` package. Callers that need both must alias one
// (typically `modelsync "github.com/stperic/zzrouter/pkg/model/sync"`).
package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

// FileChecksum represents a file with its checksum. The JSON shape is the
// wire contract exchanged with other cluster nodes during sync; field
// names must not change.
type FileChecksum struct {
	Feature string `json:"feature,omitempty"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
}

// SyncManifest represents the list of files in a model directory as
// exchanged between cluster nodes during a model transfer. It is NOT the
// same as pkg/model/integrity.ModelManifest — that type is the on-disk
// verification manifest persisted as manifest.json per model, whereas
// SyncManifest is the ephemeral wire-protocol manifest used for transfer.
type SyncManifest struct {
	Model     string         `json:"model"`
	Format    string         `json:"format"`
	Files     []FileChecksum `json:"files"`
	TotalSize int64          `json:"total_size"`
}

// RecordManifest preserves verified source roles, including for skipped files.
func RecordManifest(dir, model, source string, m *SyncManifest) error {
	files := make([]integrity.FileChecksum, 0, len(m.Files))
	for _, f := range m.Files {
		info, err := os.Stat(filepath.Join(dir, f.Name))
		if err != nil {
			return err
		}
		files = append(files, integrity.FileChecksum{RelativePath: f.Name, Size: info.Size(), ModTime: info.ModTime(), SHA256: f.SHA256, Feature: f.Feature})
	}
	return integrity.RecordFiles(dir, model, source, files...)
}

// ComputeSHA256 computes the SHA256 checksum of a file as a lowercase
// hex-encoded string.
func ComputeSHA256(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer func() { _ = file.Close() }()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("failed to compute hash: %w", err)
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Holds reports whether this node holds every file request selects, as its
// integrity manifest records them. It hashes nothing: answering "do you have
// it" for every node of a deploy must not read every node's weights.
func (s *Security) Holds(request metadata.DownloadRequest) (bool, error) {
	dir, err := s.ValidateModelDir(request.Repo)
	if err != nil {
		return false, err
	}
	manifest, err := integrity.ReadManifest(dir)
	if err != nil || manifest == nil {
		return false, err
	}
	if len(request.Files) == 0 {
		return len(manifest.Files) > 0, nil
	}
	for _, f := range request.Files {
		if _, ok := manifest.Holds(dir, f.Name, f.Size, f.SHA256); !ok {
			return false, nil
		}
	}
	return true, nil
}

// GenerateManifest generates a SyncManifest for all files in a model
// directory. The path is validated through the Security instance's
// containment checks before any files are walked.
func (s *Security) GenerateManifest(request metadata.DownloadRequest, format string) (*SyncManifest, error) {
	validPath, err := s.ValidateModelDir(request.Repo)
	if err != nil {
		return nil, err
	}

	manifest := &SyncManifest{
		Model:  request.Repo,
		Format: format,
		Files:  []FileChecksum{},
	}

	selected := map[string]metadata.DownloadFile{}
	for _, f := range request.Files {
		if f.Name == "" || filepath.IsAbs(f.Name) || strings.Contains(f.Name, "..") {
			return nil, fmt.Errorf("invalid file path %q", f.Name)
		}
		if _, duplicate := selected[f.Name]; duplicate {
			return nil, fmt.Errorf("duplicate file %q", f.Name)
		}
		selected[f.Name] = f
	}
	roles := map[string]string{}
	prior, err := integrity.ReadManifest(validPath)
	if err != nil {
		return nil, err
	}
	if prior != nil {
		for _, f := range prior.Files {
			roles[filepath.ToSlash(f.RelativePath)] = f.Feature
		}
	}
	err = filepath.Walk(validPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasPrefix(info.Name(), ".") {
			return nil
		}

		relPath, err := filepath.Rel(validPath, path)
		if err != nil {
			return err
		}
		relPath = filepath.ToSlash(relPath)
		want, chosen := selected[relPath]
		if request.Files != nil && !chosen {
			return nil
		}
		validated, err := s.ValidateModelFile(filepath.Join(request.Repo, relPath))
		if err != nil {
			return err
		}
		checksum, err := ComputeSHA256(validated)
		if err != nil {
			return fmt.Errorf("checksum %s: %w", relPath, err)
		}
		role := roles[relPath]
		if chosen {
			if want.Size != info.Size() || (want.SHA256 != "" && !strings.EqualFold(want.SHA256, checksum)) {
				return fmt.Errorf("file differs from plan: %s", relPath)
			}
			role = want.Feature
			delete(selected, relPath)
		}

		manifest.Files = append(manifest.Files, FileChecksum{
			Name:    relPath,
			Feature: role,
			Size:    info.Size(),
			SHA256:  checksum,
		})

		manifest.TotalSize += info.Size()

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to generate manifest: %w", err)
	}

	if len(selected) > 0 {
		return nil, fmt.Errorf("selected files are missing")
	}
	return manifest, nil
}

// IsModelFile reports whether a filename is a recognized model file by
// extension or by name for canonical config files.
func IsModelFile(filename string) bool {
	lower := strings.ToLower(filename)
	modelExtensions := []string{
		".gguf", ".ggml",
		".safetensors",
		".bin", ".pt", ".pth",
		".onnx",
		".mlpackage", ".mlmodel",
		".engine", ".plan",
	}

	for _, ext := range modelExtensions {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}

	configFiles := []string{
		"config.json", "tokenizer.json", "tokenizer_config.json",
		"vocab.json", "merges.txt", "special_tokens_map.json",
		"generation_config.json", "model.safetensors.index.json",
	}

	for _, name := range configFiles {
		if strings.EqualFold(filepath.Base(filename), name) {
			return true
		}
	}

	return false
}
