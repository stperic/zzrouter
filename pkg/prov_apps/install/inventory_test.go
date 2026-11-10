package install

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The fixture is a real Windows pip 26.2.2 report, with unrelated metadata removed.
func hashlessWheelReport(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("testdata/pip-report-pep658.json")
	require.NoError(t, err)
	var report map[string]any
	require.NoError(t, json.Unmarshal(data, &report))
	return report
}

func TestResolvePipReportCompletesSelectedWheelHash(t *testing.T) {
	const digest = "85ece4451f492d0c13c5dd7c13a64681a86afae63a5f347908daf103ce6d2f67"
	for _, supplied := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "supplied"}[supplied], func(t *testing.T) {
			report := hashlessWheelReport(t)
			if supplied {
				entry := report["install"].([]any)[0].(map[string]any)
				entry["download_info"].(map[string]any)["archive_info"] = map[string]any{"hashes": map[string]string{"sha256": digest}}
			}
			data, err := json.Marshal(report)
			require.NoError(t, err)
			calls := 0
			inventory, err := ResolvePipReport(t.Context(), data, map[string]string{"jinja2": ""}, func(ctx context.Context, url string) (string, error) {
				calls++
				require.NoError(t, ctx.Err())
				require.Equal(t, "https://download.pytorch.org/whl/jinja2-3.1.6-py3-none-any.whl", url)
				return digest, nil
			})
			require.NoError(t, err)
			require.Len(t, inventory, 1)
			require.Equal(t, digest, inventory[0].SHA256)
			require.Equal(t, "jinja2==3.1.6 --hash=sha256:"+digest+"\n", InventoryRequirements(inventory))
			require.Equal(t, map[bool]int{false: 1, true: 0}[supplied], calls)
		})
	}
}

func TestResolvePipReportValidatesEverythingBeforeFetching(t *testing.T) {
	for _, tc := range []string{"bad later URL", "hashless source", "bad supplied hash", "direct", "omitted root", "unexpected root", "duplicate"} {
		t.Run(tc, func(t *testing.T) {
			report := hashlessWheelReport(t)
			entry := report["install"].([]any)[0].(map[string]any)
			roots := map[string]string{"jinja2": ""}
			switch tc {
			case "bad later URL":
				report["install"] = append(report["install"].([]any), map[string]any{"metadata": map[string]string{"name": "other", "version": "1"}, "download_info": map[string]any{"url": "http://unsafe.test/other.whl"}})
			case "hashless source":
				entry["download_info"].(map[string]any)["url"] = "https://download.pytorch.org/source.tar.gz"
			case "bad supplied hash":
				entry["download_info"].(map[string]any)["archive_info"] = map[string]any{"hashes": map[string]string{"sha256": strings.Repeat("z", 64)}}
			case "direct":
				entry["is_direct"] = true
			case "omitted root":
				roots["omitted"] = ""
			case "unexpected root":
				delete(roots, "jinja2")
			case "duplicate":
				report["install"] = append(report["install"].([]any), entry)
			}
			data, err := json.Marshal(report)
			require.NoError(t, err)
			inventory, err := ResolvePipReport(t.Context(), data, roots, func(context.Context, string) (string, error) {
				t.Fatal("invalid evidence must not trigger artifact acquisition")
				return "", nil
			})
			require.Error(t, err)
			require.Nil(t, inventory)
		})
	}
}

func TestResolvePipReportHashFailuresAndCancellation(t *testing.T) {
	report := hashlessWheelReport(t)
	data, err := json.Marshal(report)
	require.NoError(t, err)
	for _, tc := range []string{"nil", "invalid result", "download failure", "cancel before", "cancel during"} {
		t.Run(tc, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			failure := errors.New("download failed")
			calls := 0
			fetch := func(context.Context, string) (string, error) {
				calls++
				switch tc {
				case "download failure":
					return "", failure
				case "cancel during":
					cancel()
					return strings.Repeat("a", 64), nil
				}
				return "invalid", nil
			}
			if tc == "nil" {
				fetch = nil
			}
			if tc == "cancel before" {
				cancel()
			}
			inventory, err := ResolvePipReport(ctx, data, map[string]string{"jinja2": ""}, fetch)
			require.Error(t, err)
			require.Nil(t, inventory)
			switch tc {
			case "nil", "cancel before":
				require.Zero(t, calls)
			default:
				require.Equal(t, 1, calls)
			}
			if strings.HasPrefix(tc, "cancel") {
				require.ErrorIs(t, err, context.Canceled)
			}
			if tc == "download failure" {
				require.ErrorIs(t, err, failure)
			}
		})
	}
}
