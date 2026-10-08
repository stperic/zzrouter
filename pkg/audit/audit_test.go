package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFileSink_EmitsJSONLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	sink, err := NewFileSink(path)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })

	ev := Event{
		Action:    ActionKeyCreated,
		Actor:     "static:admin",
		Subject:   "key-abc",
		RequestID: "req-1",
		Metadata:  map[string]any{"team": "team-1"},
	}
	if err := sink.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Ensure the event is flushed before reading.
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	if !s.Scan() {
		t.Fatalf("expected one line, got none")
	}
	var got Event
	if err := json.Unmarshal(s.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Action != ActionKeyCreated || got.Actor != "static:admin" || got.Subject != "key-abc" {
		t.Fatalf("unexpected event: %+v", got)
	}
	if got.Time.IsZero() {
		t.Fatalf("Time should be populated at emit time when caller left it zero")
	}
	if s.Scan() {
		t.Fatalf("expected EOF, got another line: %s", s.Text())
	}
}

func TestFileSink_Appends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	sink, err := NewFileSink(path)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := sink.Emit(context.Background(), Event{
			Action: ActionTeamCreated, Actor: "static:admin", Subject: "team-" + string(rune('A'+i)),
		}); err != nil {
			t.Fatalf("Emit %d: %v", i, err)
		}
	}
	_ = sink.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Count(string(data), "\n")
	if lines != 3 {
		t.Fatalf("want 3 lines, got %d: %s", lines, data)
	}
}

func TestFileSink_RejectsInvalidEvent(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewFileSink(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	cases := []struct {
		name string
		ev   Event
	}{
		{"missing action", Event{Actor: "a", Subject: "s"}},
		{"missing actor", Event{Action: ActionKeyCreated, Subject: "s"}},
		{"missing subject", Event{Action: ActionKeyCreated, Actor: "a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := sink.Emit(context.Background(), tc.ev); err == nil {
				t.Fatalf("expected validation error")
			}
		})
	}
}

func TestFileSink_ConcurrentEmit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	sink, err := NewFileSink(path)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	start := time.Now()
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = sink.Emit(context.Background(), Event{
				Action:  ActionKeyRotated,
				Actor:   "a",
				Subject: "s",
				Metadata: map[string]any{
					"i": i,
				},
			})
		}()
	}
	wg.Wait()
	_ = sink.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Count(strings.TrimRight(string(data), "\n"), "\n") + 1
	if lines != n {
		t.Fatalf("want %d lines, got %d (elapsed %s)", n, lines, time.Since(start))
	}
}

// TestActionConstants_WireFormat pins the stable on-the-wire string for
// every released Action constant. A rename here would invalidate every
// historical audit.jsonl line and break compliance tooling that greps
// for these exact strings.
func TestActionConstants_WireFormat(t *testing.T) {
	want := map[Action]string{
		ActionKeyCreated:      "key.created",
		ActionKeyUpdated:      "key.updated",
		ActionKeyRotated:      "key.rotated",
		ActionKeyDeleted:      "key.deleted",
		ActionKeyUsageReset:   "key.usage_reset",
		ActionTeamCreated:     "team.created",
		ActionTeamUpdated:     "team.updated",
		ActionTeamDeleted:     "team.deleted",
		ActionTeamUsageReset:  "team.usage_reset",
		ActionQuotaSpendReset: "quota.spend_reset",
		ActionStateSnapshot:   "state.snapshot",
		ActionStateRestore:    "state.restore",
	}
	for got, expected := range want {
		if string(got) != expected {
			t.Errorf("Action wire value drift: %q != %q", string(got), expected)
		}
	}
	if ActorStaticAdmin != "static:admin" {
		t.Errorf("ActorStaticAdmin drift: %q", ActorStaticAdmin)
	}
	if ActorSystemReconciler != "system:reconciler" {
		t.Errorf("ActorSystemReconciler drift: %q", ActorSystemReconciler)
	}
}

func TestNull(t *testing.T) {
	if err := (Null{}).Emit(context.Background(), Event{}); err != nil {
		t.Fatalf("Null.Emit must be a no-op")
	}
}

func TestLog_SwallowsSinkErrors(t *testing.T) {
	// Using Null ensures no error path; we mainly pin that Log doesn't
	// panic on a nil sink and doesn't propagate sink errors.
	Log(context.Background(), nil, Event{Action: ActionKeyCreated, Actor: "a", Subject: "s"})
	// The errSink below returns an error; Log must swallow it.
	Log(context.Background(), errSink{}, Event{Action: ActionKeyCreated, Actor: "a", Subject: "s"})
}

type errSink struct{}

func (errSink) Emit(_ context.Context, _ Event) error { return os.ErrClosed }
