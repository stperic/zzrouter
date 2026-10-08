package server

import (
	"context"
	"testing"

	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
)

// TestGroupHasCloudReplica pins the rule that let an anonymous caller
// reach a paid provider: the cloud gate runs before the resolver, so it
// sees whatever name the caller typed. A model group is not a model-cache
// entry, so a gate that consulted only the cache waved the request
// through and the group then fanned it out to a cloud replica.
//
// Any cloud replica is enough. The strategy picks the replica per call,
// so one is all it takes to bill the operator.
func TestGroupHasCloudReplica(t *testing.T) {
	// Self-contained: the rule under test is "does any replica's provider
	// report cloud", not how provider config resolves one.
	isCloud := func(provider string) bool { return provider == "openrouter" }

	groups := newStubGroupReader()
	groups.groups["all-cloud"] = &modelgroup.ModelGroup{Replicas: []modelgroup.Replica{
		{Name: "a", App: "openrouter", Model: "vendor/model"},
	}}
	groups.groups["mixed"] = &modelgroup.ModelGroup{Replicas: []modelgroup.Replica{
		{Name: "local", App: "ollama", Model: "qwen2.5:0.5b"},
		{Name: "cloud", App: "openrouter", Model: "vendor/model"},
	}}
	groups.groups["all-local"] = &modelgroup.ModelGroup{Replicas: []modelgroup.Replica{
		{Name: "a", App: "ollama", Model: "qwen2.5:0.5b"},
	}}
	groups.groups["empty"] = &modelgroup.ModelGroup{}

	cases := []struct {
		name  string
		group string
		want  bool
	}{
		{"every replica is cloud", "all-cloud", true},
		{"one cloud replica among local ones is enough", "mixed", true},
		{"all replicas local", "all-local", false},
		{"group with no replicas", "empty", false},
		{"name is not a group", "qwen2.5:0.5b", false},
		{"empty name", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := groupHasCloudReplica(groups, tc.group, isCloud); got != tc.want {
				t.Errorf("groupHasCloudReplica(%q) = %v, want %v", tc.group, got, tc.want)
			}
		})
	}
}

// TestModelIsCloudBacked_UnknowableAnswersFalse covers the paths the gate
// hits before any store is wired. It must answer false rather than
// panic: the gate only ever refuses anonymous traffic, and a model
// nobody can resolve gets a far better error from dispatch a moment
// later than "authenticate first".
func TestModelIsCloudBacked_UnknowableAnswersFalse(t *testing.T) {
	var nilServer *Server
	if nilServer.modelIsCloudBacked(context.Background(), "anything") {
		t.Error("nil server must not report a model as cloud-backed")
	}

	bare := &Server{}
	for _, name := range []string{"", "some-model"} {
		if bare.modelIsCloudBacked(context.Background(), name) {
			t.Errorf("server with no cache and no groups reported %q as cloud-backed", name)
		}
	}
}
