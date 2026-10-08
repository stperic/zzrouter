package gpu

import (
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// zzRouter routinely starts before the GPU driver does: on a Debian LXC
// worker the /dev/nvidia* nodes appeared minutes after the service came
// up, and the node then advertised no accelerator until it was restarted.
// A negative probe therefore has to expire; a positive one need not,
// because the card list is static hardware.
func TestStaleNegative(t *testing.T) {
	t.Parallel()

	driverOK := Inventory{Vendors: map[Vendor]Detection{
		VendorNVIDIA: {Vendor: VendorNVIDIA, State: StateDriverOK},
	}}
	noDriver := Inventory{Vendors: map[Vendor]Detection{
		VendorNVIDIA: {Vendor: VendorNVIDIA, State: StateHardwareNoDriver},
	}}
	absent := Inventory{Vendors: map[Vendor]Detection{
		VendorNVIDIA: {Vendor: VendorNVIDIA, State: StateAbsent},
	}}

	old := time.Now().Add(-2 * negativeRetryInterval)
	fresh := time.Now()

	tests := []struct {
		name string
		inv  Inventory
		at   time.Time
		want bool
	}{
		{"driver up, old probe: keep the cache", driverOK, old, false},
		{"driver up, fresh probe: keep the cache", driverOK, fresh, false},
		{"hardware but no driver, old probe: retry", noDriver, old, true},
		{"hardware but no driver, fresh probe: wait", noDriver, fresh, false},
		{"nothing found, old probe: retry", absent, old, true},
		{"nothing found, fresh probe: wait", absent, fresh, false},
		{"empty inventory, old probe: retry", Inventory{}, old, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := staleNegative(tt.inv, tt.at, 1); got != tt.want {
				t.Errorf("staleNegative = %v, want %v", got, tt.want)
			}
		})
	}
}

// A node that truly has no accelerator stays negative forever, so a flat
// retry interval would pay a subprocess pass a minute for the life of the
// process. The delay has to grow, without ever reaching "give up" — a GPU
// installed on day 30 must still be noticed.
func TestNegativeBackoff(t *testing.T) {
	t.Parallel()

	if got := negativeBackoff(1); got != negativeRetryInterval {
		t.Errorf("first miss: got %v, want %v", got, negativeRetryInterval)
	}
	if got := negativeBackoff(2); got != 2*negativeRetryInterval {
		t.Errorf("second miss: got %v, want %v", got, 2*negativeRetryInterval)
	}
	if got := negativeBackoff(3); got != 4*negativeRetryInterval {
		t.Errorf("third miss: got %v, want %v", got, 4*negativeRetryInterval)
	}
	if got := negativeBackoff(50); got != negativeRetryMax {
		t.Errorf("long streak should settle at the ceiling: got %v, want %v", got, negativeRetryMax)
	}
	// Never zero: a zero delay would re-probe on every single call.
	if negativeBackoff(0) <= 0 {
		t.Error("backoff must stay positive even with no recorded streak")
	}
}

// A long streak must delay the retry, not cancel it.
func TestStaleNegativeStillFiresAfterTheCeiling(t *testing.T) {
	t.Parallel()

	absent := Inventory{Vendors: map[Vendor]Detection{
		VendorNVIDIA: {Vendor: VendorNVIDIA, State: StateAbsent},
	}}
	justInside := utils.Now().Add(-negativeRetryMax / 2)
	wellPast := utils.Now().Add(-2 * negativeRetryMax)

	if staleNegative(absent, justInside, 50) {
		t.Error("a backed-off node must not re-probe before its delay elapses")
	}
	if !staleNegative(absent, wellPast, 50) {
		t.Error("a backed-off node must still re-probe once the delay elapses")
	}
}

// A node with several vendors present counts as usable if any one of
// them has a working driver stack.
func TestHasUsableGPUAcrossVendors(t *testing.T) {
	t.Parallel()

	inv := Inventory{Vendors: map[Vendor]Detection{
		VendorNVIDIA: {Vendor: VendorNVIDIA, State: StateAbsent},
		VendorAMD:    {Vendor: VendorAMD, State: StateDriverOK},
	}}
	if !inv.hasUsableGPU() {
		t.Error("expected a node with a working AMD stack to count as usable")
	}

	none := Inventory{Vendors: map[Vendor]Detection{
		VendorNVIDIA: {Vendor: VendorNVIDIA, State: StateHardwareNoDriver},
		VendorAMD:    {Vendor: VendorAMD, State: StateAbsent},
	}}
	if none.hasUsableGPU() {
		t.Error("hardware without a driver is not a usable GPU")
	}
}
