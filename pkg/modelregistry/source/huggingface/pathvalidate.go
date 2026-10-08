package huggingface

import (
	"errors"
	"net/url"
	"path"
	"strings"
)

// ErrInvalidRepoFileName: a HuggingFace siblings[].rfilename failed
// validation. parseRepoFilesJSON drops failing entries with a warning
// rather than failing the whole listing.
var ErrInvalidRepoFileName = errors.New("invalid HF repo filename")

// validateRepoFileName gates HuggingFace rfilename values before they
// reach filepath.Join. Repo authors control these strings — a poisoned
// repo can ship "../../etc/zzrouter/keys.yaml" and escape the tempdir
// without a gate. Rejects: empty/whitespace, NUL byte, backslash,
// leading "/", Windows drive letter, any ".." segment (raw or post-
// Clean), "." standalone.
func validateRepoFileName(name string) error {
	if name == "" || strings.TrimSpace(name) == "" {
		return ErrInvalidRepoFileName
	}
	if strings.ContainsRune(name, 0) {
		return ErrInvalidRepoFileName
	}
	if strings.ContainsRune(name, '\\') {
		return ErrInvalidRepoFileName
	}
	if strings.HasPrefix(name, "/") {
		return ErrInvalidRepoFileName
	}
	if len(name) >= 2 && name[1] == ':' {
		return ErrInvalidRepoFileName
	}
	// ".." check on the raw segments catches "models/../etc/passwd",
	// which path.Clean would normalize to "etc/passwd" (no escape, but
	// hostile intent — would create surprise siblings under modeldir).
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return ErrInvalidRepoFileName
		}
	}
	cleaned := path.Clean(name)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || cleaned == "." {
		return ErrInvalidRepoFileName
	}
	return nil
}

// encodeRepoFilePath URL-escapes per segment (preserving "/" so HF's
// path semantics stay intact). Closes query/fragment-injection from
// names like "foo?token=evil". Caller must validate first.
func encodeRepoFilePath(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
