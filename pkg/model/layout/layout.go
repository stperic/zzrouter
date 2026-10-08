// Package layout says what the files of a model repository are: which are
// weights, how a split GGUF's shards group into one loadable set, and which
// belong to a feature rather than to the weights. Scans, launches and
// downloads all ask it, so they agree on what "the weights" means.
package layout

import (
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/stperic/zzrouter/pkg/model/integrity"
)

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
