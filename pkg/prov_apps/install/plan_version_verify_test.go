package install

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeVersionFile drops a version marker and returns its path.
func writeVersionFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "version")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

func TestVerifyStep_FileEquals(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		expected   string
		wantPassed bool
		wantActual string
	}{
		{
			name:       "exact match passes",
			content:    "b10549",
			expected:   "b10549",
			wantPassed: true,
			wantActual: "b10549",
		},
		{
			// WriteVersionCommand appends a newline; the readers all trim.
			name:       "trailing newline is trimmed",
			content:    "b10549\n",
			expected:   "b10549",
			wantPassed: true,
			wantActual: "b10549",
		},
		{
			name:       "different version fails",
			content:    "b10520",
			expected:   "b10549",
			wantPassed: false,
			wantActual: "b10520",
		},
		{
			// The whole point of equality over substring: a prefix must not
			// satisfy the check, or a downgrade passes it.
			name:       "prefix does not satisfy",
			content:    "b10453",
			expected:   "b1045",
			wantPassed: false,
			wantActual: "b10453",
		},
		{
			name:       "empty file fails with no actual",
			content:    "",
			expected:   "b10549",
			wantPassed: false,
			wantActual: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := Step{
				Number: 1,
				Verify: StepVerify{Type: "file_equals", Path: writeVersionFile(t, tt.content), Expected: tt.expected},
			}
			got := VerifyStep(step)
			assert.Equal(t, tt.wantPassed, got.Passed, got.Message)
			assert.Equal(t, tt.wantActual, got.Actual)
		})
	}
}

// A file_equals with nothing to compare against would otherwise pass on an
// empty file, while IsInstalled reads that same empty file as not installed.
func TestVerifyStep_FileEquals_EmptyExpectedFails(t *testing.T) {
	step := Step{
		Number: 1,
		Verify: StepVerify{Type: "file_equals", Path: writeVersionFile(t, ""), Expected: ""},
	}
	got := VerifyStep(step)
	assert.False(t, got.Passed, "a check with no expected value must not report success")
	assert.Contains(t, got.Message, "no expected value")
}

func TestVerifyStep_FileEquals_Missing(t *testing.T) {
	step := Step{
		Number: 1,
		Verify: StepVerify{
			Type:     "file_equals",
			Path:     filepath.Join(t.TempDir(), "absent"),
			Expected: "b10549",
		},
	}
	got := VerifyStep(step)
	assert.False(t, got.Passed)
	assert.Empty(t, got.Actual, "a missing file must leave Actual empty, which is what separates it from a mismatch")
}

// A plan-scoped mismatch means "this node runs a different release", not
// "this install is damaged". install/verify asks the second question, so it
// must not flip AllOK — while the plan preview, which asks the first, still
// reports the step as not-installed.
func TestVerifyAll_PlanScopedMismatchIsNotDamage(t *testing.T) {
	p := &Plan{
		Provider: "llamacpp",
		Version:  "b10549",
		Steps: []Step{{
			Number: 1,
			Verify: StepVerify{
				Type:       "file_equals",
				Path:       writeVersionFile(t, "b10520"),
				Expected:   "b10549",
				PlanScoped: true,
			},
		}},
	}

	vr := p.VerifyAll()
	require.Len(t, vr.Steps, 1)
	assert.True(t, vr.AllOK, "a node on another release has a working install")
	assert.True(t, vr.Steps[0].Passed)
	assert.Contains(t, vr.Steps[0].Message, "b10520", "the message must still name what is actually there")

	// ...while the plan preview, which asks what the plan would change,
	// must report the same step as not installed. Asserting this through
	// ProbeState and not VerifyStep is the point: the preview endpoint calls
	// the whole-plan walk, so a check that only VerifyStep stays literal
	// passes while current_state still reports the step as installed.
	state := p.ProbeState()
	require.Len(t, state, 1)
	assert.False(t, state[0].Installed,
		"a reinstall that would replace b10520 with b10549 is not a no-op")
	assert.Contains(t, state[0].Detail, "b10549")
}

// Transient is forgiven under both questions. A consumed download archive is
// what guided install resumes past; reporting it as pending would re-download
// gigabytes the extract step already unpacked.
func TestProbeState_ForgivesTransient(t *testing.T) {
	p := &Plan{
		Provider: "llamacpp",
		Steps: []Step{{
			Number: 1,
			Verify: StepVerify{
				Type:      "file_exists",
				Path:      filepath.Join(t.TempDir(), "archive.tar.gz"),
				Transient: true,
			},
		}},
	}

	state := p.ProbeState()
	require.Len(t, state, 1)
	assert.True(t, state[0].Installed)
}

func TestVerifyAll_PlanScopedMissingFileIsDamage(t *testing.T) {
	p := &Plan{
		Provider: "llamacpp",
		Version:  "b10549",
		Steps: []Step{{
			Number: 1,
			Verify: StepVerify{
				Type:       "file_equals",
				Path:       filepath.Join(t.TempDir(), "absent"),
				Expected:   "b10549",
				PlanScoped: true,
			},
		}},
	}

	vr := p.VerifyAll()
	assert.False(t, vr.AllOK, "no version marker at all is a broken install under either question")
	assert.False(t, vr.Steps[0].Passed)
}

// A version difference must not blind the probe to the steps after it.
//
// The cascade is right for the integrity question — once a required step
// fails, the rest tell you nothing — but wrong for a preview, where a node
// simply running another release still has every later step's state on disk.
// ollama records its version at step 9 of 11, so before this a node one
// release behind reported its managed marker as absent while the marker sat
// on disk, with "skipped: previous step not verified" in a field documented
// as a neutral state probe.
func TestProbeState_MismatchDoesNotBlindLaterSteps(t *testing.T) {
	marker := writeVersionFile(t, "present")
	p := &Plan{
		Provider: "ollama",
		Version:  "0.32.14",
		Steps: []Step{
			{
				Number: 1,
				Verify: StepVerify{
					Type:       "file_equals",
					Path:       writeVersionFile(t, "0.32.13"),
					Expected:   "0.32.14",
					PlanScoped: true,
				},
			},
			{Number: 2, Verify: StepVerify{Type: "file_exists", Path: marker}},
		},
	}

	state := p.ProbeState()
	require.Len(t, state, 2)
	assert.False(t, state[0].Installed, "the version step differs and must say so")
	assert.True(t, state[1].Installed,
		"the marker is on disk; a version difference upstream of it is not a reason to call it absent")
	assert.NotContains(t, state[1].Detail, "skipped")

	// The integrity question is unchanged: a difference is not damage, and
	// nothing downstream is skipped either.
	vr := p.VerifyAll()
	assert.True(t, vr.AllOK)
	assert.True(t, vr.Steps[1].Passed)
}

// PlanScoped forgiveness is keyed on the one check type whose failure can only
// mean "wrong value". command_output records Actual before it looks at the
// exit status, so a command that fails while printing anything would be waved
// through as a version difference.
func TestPlanScopedForgivenessIsLimitedToFileEquals(t *testing.T) {
	p := &Plan{
		Provider: "llamacpp",
		Steps: []Step{{
			Number: 1,
			Verify: StepVerify{
				Type:       "command_output",
				Command:    "echo 'llama-server: not found' && exit 1",
				Expected:   "b10549",
				PlanScoped: true,
			},
		}},
	}

	vr := p.VerifyAll()
	assert.False(t, vr.AllOK,
		"a command that exited non-zero is a failure, not a different release")
	assert.False(t, vr.Steps[0].Passed)
}

// A step that declares no check must not read as a satisfied one. The
// llama.cpp checksum step is the deliberate case: the comparison it
// performs leaves no artifact, so there is nothing to look for
// afterwards — and reporting it green claimed an integrity check that
// had never run.
func TestUnverifiableStepIsNotReportedAsInstalled(t *testing.T) {
	plan := &Plan{
		Provider: "llamacpp",
		Steps: []Step{
			{Number: 1, Description: "Verify checksum", Command: "shasum -a 256 x"},
		},
	}

	res := VerifyStep(plan.Steps[0])
	assert.True(t, res.Passed, "nothing failed, so nothing should fail the install")
	assert.True(t, res.Unverifiable, "but it attested nothing")

	states := plan.ProbeState()
	require.Len(t, states, 1)
	assert.False(t, states[0].Installed,
		"a preview must not claim a step with no check is already done")
}

// A step that does declare a check and passes it stays reported as done.
func TestVerifiedStepStillReportsInstalled(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "version")
	require.NoError(t, os.WriteFile(marker, []byte("b10549"), 0o600))

	plan := &Plan{
		Provider: "llamacpp",
		Steps: []Step{{
			Number:      1,
			Description: "Record version",
			Verify:      StepVerify{Type: "file_equals", Path: marker, Expected: "b10549"},
		}},
	}

	states := plan.ProbeState()
	require.Len(t, states, 1)
	assert.True(t, states[0].Installed, "a real check that passed is still evidence")
}
