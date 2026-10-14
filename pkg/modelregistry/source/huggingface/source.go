package huggingface

import (
	"fmt"
	"io/fs"
	"net/url"
	"strings"
	"unicode"

	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

// ParseSource resolves an exact Hub file, without guessing other URL sources.
func ParseSource(source string) (metadata.DownloadRequest, error) {
	r := metadata.DownloadRequest{Revision: "main"}
	if strings.HasPrefix(source, "https://") {
		u, err := url.Parse(source)
		if err != nil || u.Host != "huggingface.co" || u.User != nil || (u.RawQuery != "" && u.RawQuery != "download=true") || u.ForceQuery || u.Fragment != "" {
			return r, fmt.Errorf("source must be repo#file or an HTTPS Hugging Face resolve URL")
		}
		parts := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
		if len(parts) < 5 || parts[2] != "resolve" {
			return r, fmt.Errorf("source must name a Hugging Face revision and file")
		}
		for i, part := range parts {
			parts[i], err = url.PathUnescape(part)
			if err != nil {
				return r, err
			}
		}
		r.Repo, r.Revision, r.Weights = parts[0]+"/"+parts[1], parts[3], strings.Join(parts[4:], "/")
	} else {
		var ok bool
		r.Repo, r.Weights, ok = strings.Cut(source, "#")
		if !ok {
			return r, fmt.Errorf("source must be repo#file or an HTTPS Hugging Face resolve URL")
		}
	}
	if !fs.ValidPath(r.Repo) || strings.Count(r.Repo, "/") != 1 || strings.ContainsAny(r.Repo, "\\:@?#%") || strings.IndexFunc(r.Repo, unicode.IsSpace) >= 0 || !fs.ValidPath(r.Revision) || r.Revision == "." || strings.IndexFunc(r.Revision, unicode.IsControl) >= 0 {
		return r, fmt.Errorf("invalid Hugging Face repository or revision")
	}
	if err := validateRepoFileName(r.Weights); err != nil {
		return r, err
	}
	return r, nil
}
