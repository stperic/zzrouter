package install

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

// DisposableManifest is written only after all mandatory runtime checks pass.
type DisposableManifest struct {
	Provider                      string               `json:"provider"`
	Runtime                       string               `json:"runtime"`
	PlanID                        string               `json:"plan_id"`
	Version                       string               `json:"version"`
	Recipe                        RecipeSnapshot       `json:"resolved_install"`
	PythonVersion                 string               `json:"python_version"`
	RuntimeEnvironmentFingerprint string               `json:"runtime_environment_fingerprint"`
	Checks                        schema.RuntimeChecks `json:"checks"`
	Inventory                     []ResolvedPackage    `json:"dependency_inventory"`
}

var planIdentityPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// ValidPlanID refuses paths and caller-selected executable names.
func ValidPlanID(identity string) bool { return planIdentityPattern.MatchString(identity) }

// DisposableDir derives a destination below the selected managed runtime.
func DisposableDir(runtime, identity string) string {
	return filepath.Join(fsroot.ProviderDir(runtime), ".disposable", identity)
}

func WriteDisposableManifest(runtime, identity string, manifest DisposableManifest) error {
	if !ValidPlanID(identity) || manifest.PlanID != identity || manifest.Runtime != runtime {
		return fmt.Errorf("invalid disposable identity")
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	path := filepath.Join(DisposableDir(runtime, identity), "disposable.json")
	if err := os.WriteFile(path+".tmp", data, 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// ReadDisposableManifest refuses incomplete installs and never falls back to the active runtime.
func ReadDisposableManifest(runtime, identity string) (*DisposableManifest, error) {
	if !ValidPlanID(identity) {
		return nil, fmt.Errorf("invalid disposable_plan_id")
	}
	path := filepath.Join(DisposableDir(runtime, identity), "disposable.json")
	if err := fsroot.VerifySymlinkSafe(path); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("disposable runtime not verified: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("disposable manifest exceeds bound")
	}
	var manifest DisposableManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	if manifest.PlanID != identity || manifest.Runtime != runtime {
		return nil, fmt.Errorf("disposable manifest identity mismatch")
	}
	return &manifest, nil
}
