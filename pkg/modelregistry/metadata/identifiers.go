package metadata

import (
	"io/fs"
	"strings"
	"unicode"
)

// ValidHuggingFaceID permits one repository owner and name, never URL syntax.
func ValidHuggingFaceID(id string) bool {
	return len(id) <= 512 && fs.ValidPath(id) && strings.Count(id, "/") == 1 && !strings.ContainsAny(id, "\\:@?#%") && strings.IndexFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}
