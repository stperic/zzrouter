package harness

import (
	"strings"
	"testing"
)

func TestRunModelNames_IncludesTheNameARunIsActuallyListedUnder(t *testing.T) {
	// The chat fixture, verbatim from the Nightly profile.
	spec := ModelSpec{
		ID:     "qwen2.5-1.5b-instruct-gguf",
		Source: "huggingface://Qwen/Qwen2.5-1.5B-Instruct-GGUF",
		File:   "qwen2.5-1.5b-instruct-q4_k_m.gguf",
	}

	names := RunModelNames(spec)

	// The name the launch is made with, and the name the run comes back
	// under once the node has resolved it to the local file. A readiness
	// wait that knows only the first one polls until its deadline while
	// the model sits there serving.
	for _, want := range []string{
		"Qwen/Qwen2.5-1.5B-Instruct-GGUF",
		"qwen2.5-1.5b-instruct-q4_k_m",
	} {
		if !contains(names, want) {
			t.Errorf("RunModelNames() = %v, missing %q", names, want)
		}
	}
}

func TestRunModelNames_HasNoEmptyOrRepeatedEntries(t *testing.T) {
	// A cloud fixture carries no source id and no file.
	spec := ModelSpec{ID: "openrouter/qwen-2.5-7b-instruct", Source: "cloud"}

	names := RunModelNames(spec)
	if len(names) != 1 || names[0] != spec.ID {
		t.Fatalf("RunModelNames() = %v, want just the fixture id", names)
	}

	// An empty name would prefix-match every run row.
	for _, n := range RunModelNames(ModelSpec{}) {
		if n == "" {
			t.Error("RunModelNames returned an empty name, which matches every run")
		}
	}
}

func TestRegistryModelID(t *testing.T) {
	tests := []struct {
		name string
		spec ModelSpec
		want string
	}{
		{
			name: "source scheme is stripped",
			spec: ModelSpec{ID: "fixture-id", Source: "huggingface://Qwen/Qwen2.5-1.5B-Instruct-GGUF"},
			want: "Qwen/Qwen2.5-1.5B-Instruct-GGUF",
		},
		{
			name: "ollama source",
			spec: ModelSpec{ID: "fixture-id", Source: "ollama://llama3.1:8b"},
			want: "llama3.1:8b",
		},
		{
			name: "cloud falls back to the fixture id",
			spec: ModelSpec{ID: "openrouter/qwen", Source: "cloud"},
			want: "openrouter/qwen",
		},
		{
			name: "no source falls back to the fixture id",
			spec: ModelSpec{ID: "fixture-id"},
			want: "fixture-id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RegistryModelID(tt.spec); got != tt.want {
				t.Errorf("RegistryModelID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHasAnyPrefix_IgnoresTheEmptyString(t *testing.T) {
	if hasAnyPrefix("anything-at-all", []string{""}) {
		t.Error("an empty prefix matched a run row")
	}
	// The auto_deploy chain appends a "#variant" hint before launch.
	if !hasAnyPrefix("qwen2.5-1.5b-instruct-q4_k_m#Q4_K_M", []string{"qwen2.5-1.5b-instruct-q4_k_m"}) {
		t.Error("a run carrying a #variant hint did not match its model name")
	}
	if hasAnyPrefix("some-other-model", []string{"qwen2.5"}) {
		t.Error("an unrelated model matched")
	}
}

func TestFileStem(t *testing.T) {
	for in, want := range map[string]string{
		"qwen2.5-1.5b-instruct-q4_k_m.gguf": "qwen2.5-1.5b-instruct-q4_k_m",
		"nested/dir/model.Q4_K_M.gguf":      "model.Q4_K_M",
		"":                                  "",
	} {
		if got := fileStem(in); got != want {
			t.Errorf("fileStem(%q) = %q, want %q", in, got, want)
		}
	}
	if strings.Contains(fileStem("a/b/c.gguf"), "/") {
		t.Error("fileStem kept a directory component")
	}
}

// The 202 envelope names the run "id". Reading it as "instance_id" left
// the field empty on every launch, so every cleanup guarded on it was a
// no-op and runs piled up on the lab between suites.
//
// The fix for that was checked against the INTERNAL shape, which puts
// the fields at the top level — a body POST /zzrouter/v1/runs never
// sends. It wraps them in `data`, so the field went on being empty on
// exactly the path the suites use, and the test agreed with the body we
// had written rather than the one the server writes. Both shapes are
// pinned here now.
func TestLaunchRunResponse_ReadsTheRunIDTheServerSends(t *testing.T) {
	const publicBody = `{"success":true,"message":"Run launch accepted; subscribe to job_id for readiness",` +
		`"data":{"accepted":true,"id":"ccffa3aa9877","job_id":"run_f4f26329b24e6cc4",` +
		`"provider":"llamacpp","launch_mode":"native","status":"starting","port":8080}}`
	const internalBody = `{"accepted":true,"id":"22c826f39f1a","job_id":"run_09183e93739357bb",` +
		`"provider":"llamacpp","launch_mode":"native","status":"starting","node":"worker-1"}`

	for _, tc := range []struct {
		name, body, wantID, jobID string
	}{
		{"public envelope", publicBody, "ccffa3aa9877", "run_f4f26329b24e6cc4"},
		{"internal reply", internalBody, "22c826f39f1a", "run_09183e93739357bb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeLaunchResponse([]byte(tc.body), tc.jobID)
			if got.InstanceID != tc.wantID {
				t.Errorf("InstanceID = %q, want the run id the server sent (%q)", got.InstanceID, tc.wantID)
			}
			if got.JobID != tc.jobID {
				t.Errorf("JobID = %q, want %q", got.JobID, tc.jobID)
			}
			if got.Provider != "llamacpp" {
				t.Errorf("Provider = %q, want llamacpp", got.Provider)
			}
		})
	}
}
