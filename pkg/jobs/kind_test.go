package jobs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKindPrefix(t *testing.T) {
	cases := map[Kind]string{
		KindDownload:     "dl_",
		KindInstall:      "inst_",
		KindUpdate:       "upd_",
		KindSync:         "sync_",
		KindInferenceLog: "inflog_",
		KindService:      "svc_",
		KindCalibration:  "cal_",
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
}

func TestCalibrationJobPolicyCoversMaximumDuration(t *testing.T) {
	policy := KindCalibration.DefaultRingPolicy()
	if policy.Mode != RingBounded || policy.Size == 0 || policy == KindService.DefaultRingPolicy() {
		t.Fatalf("calibration requires a dedicated bounded ring, got %+v", policy)
	}
	if timeout := KindCalibration.DefaultInactivityTimeout(); timeout <= 9*1800*time.Second {
		t.Fatalf("calibration inactivity timeout %s cannot cover a warm-up and eight maximum steps", timeout)
	}
}

func TestCalibrationReportsAreOmittedOnlyFromLists(t *testing.T) {
	registry, _ := newTestRegistry(t)
	const reportBytes = 40 << 10
	var id string
	for range 30 {
		handle, err := registry.StartDetached(KindCalibration, "", Meta{"report": strings.Repeat("x", reportBytes)})
		require.NoError(t, err)
		id = handle.ID()
		handle.Done()
	}
	list := registry.List(KindCalibration)
	require.Len(t, list, 30)
	payload, err := json.Marshal(list)
	require.NoError(t, err)
	require.Less(t, len(payload), 1<<20)
	require.NotContains(t, string(payload), `"report"`)

	item, err := registry.Get(id)
	require.NoError(t, err)
	require.Len(t, item.Meta["report"], reportBytes)
	sub, err := registry.Subscribe(id, SubscribeOptions{From: &item.Seq})
	require.NoError(t, err)
	defer sub.Unsubscribe()
	terminal := <-sub.Events()
	require.True(t, terminal.Phase.IsTerminal())
	require.Len(t, terminal.Meta["report"], reportBytes)
}
