// Package layout says what the files of a model repository are: which are
// weights, how a split GGUF's shards group into one loadable set, and which
// belong to a feature rather than to the weights. Scans, launches and
// downloads all ask it, so they agree on what "the weights" means.
package layout

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/stperic/zzrouter/pkg/model/integrity"
)

// ErrIncomplete means a model lacks the weights needed to serve it.
var ErrIncomplete = errors.New("model weights incomplete")

// Complete reports whether weights and every indexed shard are present, excluding feature files.
func Complete(modelPath string) (bool, error) {
	info, err := os.Stat(modelPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() && IsGGUF(modelPath) {
		_, ok := VariantOf(modelPath)
		return ok, nil
	}
	if !info.IsDir() {
		switch strings.ToLower(filepath.Ext(modelPath)) {
		case ".safetensors", ".bin", ".npz":
			return Complete(filepath.Dir(modelPath))
		}
		return info.Mode().IsRegular() && info.Size() > 0, nil
	}
	files, err := OnDisk(modelPath)
	if err != nil {
		return false, err
	}
	weights := make(map[string]bool)
	var indexes []string
	for _, f := range files {
		if isTensorWeights(f.Path) {
			weights[f.Path] = f.Size > 0
		}
		if strings.HasSuffix(f.Path, ".safetensors.index.json") || strings.HasSuffix(f.Path, ".bin.index.json") {
			indexes = append(indexes, f.Path)
		}
	}
	for _, index := range indexes {
		data, err := os.ReadFile(filepath.Join(modelPath, filepath.FromSlash(index)))
		if err != nil {
			return false, err
		}
		var shards struct {
			Weights map[string]string `json:"weight_map"`
		}
		if err := json.Unmarshal(data, &shards); err != nil || len(shards.Weights) == 0 {
			return false, nil
		}
		for _, shard := range shards.Weights {
			if !fs.ValidPath(shard) || !weights[path.Join(path.Dir(index), shard)] {
				return false, nil
			}
		}
	}
	for _, present := range weights {
		if present {
			return true, nil
		}
	}
	return len(GGUFVariants(files)) > 0, nil
}

// File is one file of a repository, by its slash-separated path inside it.
type File struct {
	Path string
	Size int64
}

// Variant is one loadable set of GGUF weights: a single file, or every
// shard of a split one. Files[0] is the file an engine is pointed at; a
// GGUF loader finds the other shards beside it.
type Variant struct {
	Name  string
	Files []File
}

// Size is the variant's total size in bytes.
func (v Variant) Size() int64 {
	var n int64
	for _, f := range v.Files {
		n += f.Size
	}
	return n
}

// shardRE matches the gguf-split naming: <prefix>-00001-of-00003.gguf.
var shardRE = regexp.MustCompile(`^(.*)-(\d{5})-of-(\d{5})\.gguf$`)

// IsGGUF reports whether p names a GGUF file.
func IsGGUF(p string) bool { return strings.EqualFold(path.Ext(p), ".gguf") }

// GGUFVariants groups the GGUF files among files into variants, sorted by
// name. A split set appears once, and only when every shard is present.
func GGUFVariants(files []File) []Variant {
	type set struct {
		name   string
		total  int
		shards map[int]File
	}
	sets := map[string]*set{}
	var out []Variant
	for _, f := range files {
		if !IsGGUF(f.Path) {
			continue
		}
		m := shardRE.FindStringSubmatch(f.Path)
		if m == nil {
			out = append(out, Variant{Name: stem(f.Path), Files: []File{f}})
			continue
		}
		index, _ := strconv.Atoi(m[2])
		total, _ := strconv.Atoi(m[3])
		key := m[1] + "/" + m[3]
		s, ok := sets[key]
		if !ok {
			s = &set{name: path.Base(m[1]), total: total, shards: map[int]File{}}
			sets[key] = s
		}
		s.shards[index] = f
	}
	for _, s := range sets {
		v := Variant{Name: s.name}
		for i := 1; i <= s.total; i++ {
			if f, ok := s.shards[i]; ok {
				v.Files = append(v.Files, f)
			}
		}
		if len(v.Files) == s.total {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Files[0].Path < out[j].Files[0].Path })
	return out
}

// VariantOf returns the complete variant the GGUF file at path belongs to,
// its Files relative to path's directory. Feature files, excluded files and
// shards of an incomplete set belong to none.
func VariantOf(path string, excluded ...string) (Variant, bool) {
	if Matches(filepath.Base(path), excluded) {
		return Variant{}, false
	}
	files, err := InDir(filepath.Dir(path), excluded...)
	if err != nil {
		return Variant{}, false
	}
	base := filepath.Base(path)
	for _, v := range GGUFVariants(files) {
		for _, f := range v.Files {
			if f.Path == base {
				return v, true
			}
		}
	}
	return Variant{}, false
}

// Pick returns the variant hint names: one holding a file at that path,
// then the first, by path, whose name contains it (both case-insensitive),
// then the only variant when there is no hint.
func Pick(variants []Variant, hint string) (Variant, bool) {
	if hint != "" {
		for _, v := range variants {
			for _, f := range v.Files {
				if strings.EqualFold(f.Path, hint) {
					return v, true
				}
			}
		}
		lower := strings.ToLower(hint)
		for _, v := range variants {
			if strings.Contains(strings.ToLower(v.Name), lower) {
				return v, true
			}
		}
		return Variant{}, false
	}
	if len(variants) == 1 {
		return variants[0], true
	}
	return Variant{}, false
}

// Matches reports whether p matches any of the globs, tried against the
// whole path and against its last element.
func Matches(p string, globs []string) bool {
	for _, g := range globs {
		if ok, _ := path.Match(g, p); ok {
			return true
		}
		if ok, _ := path.Match(g, path.Base(p)); ok {
			return true
		}
	}
	return false
}

// FirstMatch returns the file the globs pick: the first glob, in order,
// that matches any file wins, so a list states a preference.
func FirstMatch(files []File, globs []string) (File, bool) {
	for _, g := range globs {
		for _, f := range files {
			if Matches(f.Path, []string{g}) {
				return f, true
			}
		}
	}
	return File{}, false
}

func stem(p string) string {
	base := path.Base(p)
	return strings.TrimSuffix(base, path.Ext(base))
}

// OnDisk lists the files under a model directory, slash-separated and
// relative to it, leaving out zzRouter's bookkeeping and every file its
// manifest records as belonging to a feature, plus declared exclusion globs.
func OnDisk(dir string, excluded ...string) ([]File, error) {
	feature, err := featureFiles(dir)
	if err != nil {
		return nil, err
	}
	var out []File
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || integrity.IsBookkeeping(d.Name()) {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if feature[rel] || Matches(rel, excluded) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, File{Path: rel, Size: info.Size()})
		return nil
	})
	return out, err
}

// InDir lists the files directly in dir the way OnDisk does, without
// descending. It is best effort: an unreadable manifest tags nothing and an
// unreadable entry is skipped, so one bad file never hides its neighbours.
func InDir(dir string, excluded ...string) ([]File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	feature, _ := featureFiles(dir)
	var out []File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || integrity.IsBookkeeping(name) || feature[name] || Matches(name, excluded) {
			continue
		}
		if info, err := e.Info(); err == nil {
			out = append(out, File{Path: name, Size: info.Size()})
		}
	}
	return out, nil
}

// featureFiles reads which files dir's manifest assigns to a feature.
func featureFiles(dir string) (map[string]bool, error) {
	manifest, err := integrity.ReadManifest(dir)
	if err != nil {
		return nil, err
	}
	feature := map[string]bool{}
	if manifest != nil {
		for _, f := range manifest.Files {
			if f.Feature != "" {
				feature[filepath.ToSlash(f.RelativePath)] = true
			}
		}
	}
	return feature, nil
}

// Weights returns the selected weights and their sizes using the shared shard and feature rules.
func Weights(modelPath string, excluded []string) ([]File, error) {
	info, err := os.Stat(modelPath)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() && IsGGUF(modelPath) {
		variant, ok := VariantOf(modelPath, excluded...)
		if !ok {
			return nil, ErrIncomplete
		}
		return variant.Files, nil
	}
	dir := modelPath
	if !info.IsDir() {
		dir = filepath.Dir(modelPath)
	}
	files, err := OnDisk(dir, excluded...)
	if err != nil {
		return nil, err
	}
	var weights []File
	for _, file := range files {
		if isTensorWeights(file.Path) {
			weights = append(weights, file)
		}
	}
	return weights, nil
}

func isTensorWeights(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".safetensors", ".bin", ".npz":
		return true
	}
	return false
}
