package detect

import (
	"github.com/stperic/zzrouter/pkg/prov_apps/port"
)

// CheckPortListening checks if a port has a listening process.
func CheckPortListening(p int) bool {
	return port.IsPortInUse(p)
}
