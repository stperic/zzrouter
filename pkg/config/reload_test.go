package config

import "testing"

// TestReloadReport_Predicates locks the NeedsRestart / HasRejection
// semantics. A regression here changes what the HTTP reload handler
// surfaces to operators — so the contract is worth a small test.
func TestReloadReport_Predicates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		dispositions []ReloadDisposition
		needsRestart bool
		hasReject    bool
	}{
		{"empty", nil, false, false},
		{"all applied", []ReloadDisposition{DispositionApplied, DispositionApplied}, false, false},
		{"ignored only", []ReloadDisposition{DispositionIgnored}, false, false},
		{"mixed with restart", []ReloadDisposition{DispositionApplied, DispositionRequiresRestart}, true, false},
		{"mixed with reject", []ReloadDisposition{DispositionApplied, DispositionRejected}, false, true},
		{"restart + reject", []ReloadDisposition{DispositionRequiresRestart, DispositionRejected}, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &ReloadReport{}
			for i, d := range tc.dispositions {
				r.Add("listener-"+string(rune('a'+i)), d)
			}
			if got := r.NeedsRestart(); got != tc.needsRestart {
				t.Errorf("NeedsRestart() = %v, want %v", got, tc.needsRestart)
			}
			if got := r.HasRejection(); got != tc.hasReject {
				t.Errorf("HasRejection() = %v, want %v", got, tc.hasReject)
			}
		})
	}
}

// TestReloadDisposition_String locks the wire strings since LogSummary
// emits them and ops/alerting may grep on them.
func TestReloadDisposition_String(t *testing.T) {
	t.Parallel()
	cases := map[ReloadDisposition]string{
		DispositionApplied:         "applied",
		DispositionIgnored:         "ignored",
		DispositionRequiresRestart: "requires_restart",
		DispositionRejected:        "rejected",
	}
	for d, want := range cases {
		if got := d.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(d), got, want)
		}
	}
}
