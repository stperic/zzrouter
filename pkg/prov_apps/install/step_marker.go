package install

import (
	"time"

	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
)

// WriteManagedMarkerStep returns a plan step that writes the .managed marker file.
// The .managed file distinguishes zzRouter-installed providers from independently
// installed ones; it gates uninstall and is checked by fsroot.ManagedMarkerExists.
func WriteManagedMarkerStep(number int, providerName string) Step {
	markerFile := fsroot.ManagedMarkerPath(providerName)
	return Step{
		Number:      number,
		Description: "Mark installation as managed by zzRouter",
		Command:     fsroot.WriteVersionCommand("managed", markerFile),
		Timeout:     5 * time.Second,
		Verify:      StepVerify{Type: "file_exists", Path: markerFile},
	}
}
