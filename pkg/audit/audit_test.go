package audit

import (
	"math"
	"testing"
)

// TestValidateRefusesIncompleteEvent refuses an event a review could not trust.
func TestValidateRefusesIncompleteEvent(t *testing.T) {
	t.Parallel()
	for name, ev := range map[string]Event{
		"missing action":  {Actor: "a", Subject: "s"},
		"missing actor":   {Action: ActionKeyCreated, Subject: "s"},
		"missing subject": {Action: ActionKeyCreated, Actor: "a"},
	} {
		if err := ev.Validate(); err == nil {
			t.Errorf("%s: event accepted", name)
		}
	}
}

// TestActionConstants_WireFormat pins the stable on-the-wire string for
// every released Action constant. A rename here would invalidate recorded
// history and break compliance tooling that greps for these exact strings.
func TestActionConstants_WireFormat(t *testing.T) {
	want := map[Action]string{
		ActionKeyCreated:     "key.created",
		ActionKeyUpdated:     "key.updated",
		ActionKeyRotated:     "key.rotated",
		ActionKeyDeleted:     "key.deleted",
		ActionKeyUsageReset:  "key.usage_reset",
		ActionTeamCreated:    "team.created",
		ActionTeamUpdated:    "team.updated",
		ActionTeamDeleted:    "team.deleted",
		ActionTeamUsageReset: "team.usage_reset",
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

func TestValidateRefusesMetadataJSONWouldAlter(t *testing.T) {
	t.Parallel()
	valid := Event{Action: ActionKeyCreated, Actor: "a", Subject: "s", Metadata: map[string]any{"purpose": "setup", "n": 1.5, "list": []any{"x"}}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid metadata refused: %v", err)
	}
	for name, metadata := range map[string]map[string]any{
		"invalid UTF-8 key":    {"bad\xffname": 1},
		"invalid UTF-8 nested": {"list": []any{map[string]any{"k": "bad\xff"}}},
		"NaN":                  {"n": math.NaN()},
		"unencodable":          {"c": make(chan int)},
	} {
		ev := valid
		ev.Metadata = metadata
		if err := ev.Validate(); err == nil {
			t.Errorf("%s: metadata accepted", name)
		}
	}
}
