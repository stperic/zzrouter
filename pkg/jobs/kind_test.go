package jobs

import "testing"

func TestKindPrefix(t *testing.T) {
	cases := map[Kind]string{
		KindDownload:     "dl_",
		KindInstall:      "inst_",
		KindUpdate:       "upd_",
		KindSync:         "sync_",
		KindInferenceLog: "inflog_",
		Kind("unknown"):  "job_",
	}
	for k, want := range cases {
		if got := k.idPrefix(); got != want {
			t.Errorf("%s: prefix = %q, want %q", k, got, want)
		}
	}
}

func TestKindDefaultRingPolicy(t *testing.T) {
	cases := map[Kind]RingMode{
		KindDownload:     RingBounded,
		KindInstall:      RingBounded,
		KindUpdate:       RingBounded,
		KindSync:         RingTerminal,
		KindInferenceLog: RingFirehose,
		Kind("unknown"):  RingBounded,
	}
	for k, mode := range cases {
		if got := k.DefaultRingPolicy(); got.Mode != mode {
			t.Errorf("%s: mode = %v, want %v", k, got.Mode, mode)
		}
	}
}

func TestKindInactivityZeroForFirehose(t *testing.T) {
	if KindInferenceLog.DefaultInactivityTimeout() != 0 {
		t.Errorf("inference-log must have zero inactivity timeout (firehose never idle)")
	}
	if KindDownload.DefaultInactivityTimeout() <= 0 {
		t.Errorf("download must have non-zero inactivity timeout")
	}
}
