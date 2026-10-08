package metadata

// TreeFileEntry represents a file entry in a HuggingFace repo, surfaced
// to callers that need per-file sizes (the registries-discovery
// endpoint in particular).
//
// Implementation note: despite the name, the current HF connector
// populates this type manually from the CARD API with `?blobs=true`
// (hits `/api/models/<id>` and walks the `siblings` array), not from
// the dedicated tree endpoint. The `json:"path"` tag is a forward-compat
// affordance for decoding `/api/models/<id>/tree/<rev>` JSON directly,
// but nothing in-repo currently does that — callers receive instances
// constructed by struct literal in `getRepoFiles`.
//
// Distinct from `pkg/modelregistry/search.RepoFileInfo` because that
// type decodes HF model-card siblings via `json.Unmarshal` into
// `ModelInfo{Siblings: []hfSibling}`, whereas this type exists so the
// download/variants flow can pass files around without pulling in the
// search package's decoder DTO. Do not unify without reconciling the
// two call-site expectations — see
// `docs/plan_search_modelregistry_consolidation.md` for the arc that
// disambiguated these types.
type TreeFileEntry struct {
	Name string `json:"path"`
	Size int64  `json:"size"`
	// SHA256 is the LFS oid from HuggingFace's card API (siblings[].lfs.oid).
	// Populated only for files served via Git LFS (every GGUF/safetensors of
	// any size). Empty for plain repo files (config.json, tokenizer.json,
	// etc.) which HF doesn't hash. Verified at download time when set.
	SHA256 string `json:"sha256,omitempty"`
}

// DownloadRequest says what to fetch from a model repository.
type DownloadRequest struct {
	// Force re-fetches the selected file set, even when already recorded locally.
	Force bool `json:"force"`
	// Files is the coordinator's resolved file set. Nil resolves from the registry.
	Files []DownloadFile `json:"files"`
	// Repo is the repository id (org/name).
	Repo string `json:"repo"`
	// Weights names the weights to fetch: a file path, one shard of a
	// split set (the whole set comes), or a name fragment such as a quant
	// tag. Empty fetches the whole repository.
	Weights string `json:"weights"`
	// Features declares every feature the engine knows, each with its file
	// globs in order of preference. A file any of them matches is never
	// weights, and is fetched only for a feature named in Want.
	Features map[string][]string `json:"features"`
	// Want names the features to fetch along with the weights.
	Want []string `json:"want"`
	// Optional names selected defaults that may be absent from a text-only repo.
	Optional []string `json:"optional"`
}

// DownloadFile is a concrete repository file and its feature role.
type DownloadFile struct {
	TreeFileEntry
	Feature string `json:"feature,omitempty"`
}
