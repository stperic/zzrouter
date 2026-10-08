package logs

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestFetchTail_DecodesEnvelope(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zzrouter/v1/runs/r_1/logs" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"log_file":    "/tmp/r_1.log",
				"total_lines": 3,
				"lines":       []string{"line-1", "line-2", "line-3"},
				"count":       3,
				"truncated":   false,
			},
		})
	})

	got, err := FetchTail(context.Background(), c, "r_1", 100)
	if err != nil {
		t.Fatalf("FetchTail: %v", err)
	}
	if len(got.Lines) != 3 {
		t.Fatalf("want 3 lines, got %d", len(got.Lines))
	}
	if got.TotalLines != 3 {
		t.Fatalf("want total 3, got %d", got.TotalLines)
	}
	if got.LastLine != "line-3" {
		t.Fatalf("want LastLine=line-3, got %q", got.LastLine)
	}
	if got.LogFile != "/tmp/r_1.log" {
		t.Fatalf("want log file, got %q", got.LogFile)
	}
}

func TestFetchTail_EmptyRunIDErrors(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {})
	_, err := FetchTail(context.Background(), c, "", 100)
	if err == nil {
		t.Fatal("want error for empty run id")
	}
}

func TestFetchTail_NonOKStatus(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	_, err := FetchTail(context.Background(), c, "r_1", 100)
	if err == nil {
		t.Fatal("want error for non-200")
	}
}

func TestFetchTail_DefaultLines(t *testing.T) {
	var seenLines string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenLines = r.URL.Query().Get("lines")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"lines": []string{}},
		})
	})
	_, err := FetchTail(context.Background(), c, "r_1", 0)
	if err != nil {
		t.Fatalf("FetchTail: %v", err)
	}
	if seenLines != "250" {
		t.Fatalf("want lines=250 (DefaultTailLines), got %q", seenLines)
	}
}
