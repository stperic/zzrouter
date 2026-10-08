package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/security"
)

// streamTailLines caps the in-memory tail buffer a streaming step
// retains for error reporting. Long pip installs emit tens of
// thousands of lines; only the tail is useful for diagnosing failures.
const streamTailLines = 40

// tailBuffer is a thread-safe ring-ish slice used by the streaming +
// pty exec paths to keep the last N lines of subprocess output. On a
// non-zero exit, the buffer's contents are joined into the error so
// operators see what the process said right before it died.
type tailBuffer struct {
	mu    sync.Mutex
	lines []string
}

func (t *tailBuffer) push(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if len(t.lines) > streamTailLines {
		t.lines = t.lines[len(t.lines)-streamTailLines:]
	}
}

func (t *tailBuffer) joined() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(strings.Join(t.lines, "\n"))
}

// runStepStreaming executes cmd with stdout+stderr merged and forwards
// each full line to onLine. Retains the final streamTailLines lines so
// a non-zero exit can embed meaningful context in the returned error.
// Reader side uses a large scanner buffer so long pip progress lines
// (with unicode bar glyphs) don't truncate.
func runStepStreaming(ctx context.Context, cmd *exec.Cmd, onLine func(string)) error {
	output := &stepOutput{onLine: onLine}
	cmd.Stdout, cmd.Stderr = output, output
	cmd.WaitDelay = 2 * time.Second
	err := process.RunOwnedCommand(ctx, cmd)
	output.flush()
	if err != nil {
		return fmt.Errorf("%w: %s", err, security.RedactSensitive(output.tail.joined()))
	}
	return nil
}

type stepOutput struct {
	onLine  func(string)
	pending string
	tail    tailBuffer
}

func (w *stepOutput) Write(data []byte) (int, error) {
	n := len(data)
	for len(data) > 0 {
		index := bytes.IndexAny(data, "\r\n")
		if index < 0 {
			w.pending += string(data)
			if len(w.pending) > 64<<10 {
				w.pending = w.pending[len(w.pending)-(64<<10):]
			}
			break
		}
		w.pending += string(data[:index])
		w.flush()
		data = data[index+1:]
	}
	return n, nil
}
func (w *stepOutput) flush() {
	if w.pending != "" {
		w.tail.push(w.pending)
		if w.onLine != nil {
			w.onLine(w.pending)
		}
		w.pending = ""
	}
}

// Plan is a structured install recipe. It can be either:
//   - Executed automatically by Install() (zzrouter runs each step)
//   - Returned to the user via InstallPlan() for manual execution
//
// Same source of truth for both modes.
type Plan struct {
	Preflight         func(context.Context) error `json:"-"`
	Inventory         []ResolvedPackage           `json:"dependency_inventory,omitempty"`
	SupplyChainNotice string                      `json:"supply_chain_notice,omitempty"`
	Disposable        bool                        `json:"disposable,omitempty"`
	PlanID            string                      `json:"plan_id,omitempty"`
	Recipe            *RecipeSnapshot             `json:"resolved_install,omitempty"`
	Environment       []string                    `json:"-"`
	Rollback          func() error                `json:"-"`
	Provider          string                      `json:"provider"`
	Version           string                      `json:"version"`
	Platform          fsroot.Platform             `json:"platform"`
	Action            string                      `json:"action"` // "install", "upgrade", "uninstall"
	InstallDir        string                      `json:"install_dir"`
	Steps             []Step                      `json:"steps"`
	CurrentState      []StepState                 `json:"current_state,omitempty"` // Pre-install probe of each step's current state (populated by server on plan preview)
}

// StepState reports the current on-disk state of a step before install runs.
// It is a state probe, not a pass/fail judgment — `Installed: false` is the
// expected state on a fresh machine, not a failure.
type StepState struct {
	Step      int    `json:"step"`
	Installed bool   `json:"installed"`
	Detail    string `json:"detail,omitempty"`
}

// stateFromResult converts a VerifyStep result into the neutrally-framed
// StepState used on plan-preview responses.
func stateFromResult(r StepResult) StepState {
	// A step that declares no check is not evidence of anything, so a
	// preview must not claim it is already done. It reads as "will run",
	// which on a fresh machine is exactly right and on an installed one
	// is the honest answer to a question nobody can answer.
	return StepState{Step: r.Step, Installed: r.Passed && !r.Unverifiable, Detail: r.Message}
}

// StatesFromVerify converts a VerifyResult's per-step outcomes into StepStates
// for embedding in a plan-preview response.
func StatesFromVerify(results []StepResult) []StepState {
	states := make([]StepState, len(results))
	for i, r := range results {
		states[i] = stateFromResult(r)
	}
	return states
}

// Step is a single step in an install plan.
type Step struct {
	ExecutionEnvironment []string          `json:"-"`
	ServiceAction        ServiceAction     `json:"service_action,omitempty"`
	Number               int               `json:"step"`
	Description          string            `json:"description"`
	Command              string            `json:"command"`
	Stdin                string            `json:"stdin,omitempty"`
	RunAs                string            `json:"run_as,omitempty"`
	WorkingDir           string            `json:"working_dir,omitempty"`
	Env                  map[string]string `json:"env,omitempty"`
	Notes                string            `json:"notes,omitempty"`
	Verify               StepVerify        `json:"verify"`
	Timeout              time.Duration     `json:"timeout,omitempty"`  // Per-step timeout; 0 = use parent context deadline
	Optional             bool              `json:"optional,omitempty"` // If true, step failure is logged but does not abort the install

	// PreExec runs before the shell command in automated mode only.
	// Skipped in guided mode (not serialized to JSON).
	PreExec func(ctx context.Context) error `json:"-"`

	// PostExec runs after the shell command in automated mode only.
	// Use for checksum verification, symlink safety checks, etc.
	PostExec func(ctx context.Context) error `json:"-"`

	// StdoutLine, when non-nil, enables line-by-line streaming of the
	// command's merged stdout/stderr instead of the default
	// CombinedOutput buffered read. Each line is also retained in an
	// internal tail buffer so error messages include the last N lines.
	// Used by pip-backed installers to parse "Collecting/Downloading"
	// announcements into progress updates. Skipped in guided mode.
	StdoutLine func(line string) `json:"-"`

	// UsePTY, when true with StdoutLine set, allocates a pseudo-terminal
	// for the child process on Unix so pip (and other tools that gate
	// their progress bar on isatty) emit live within-file byte updates
	// instead of silent line-oriented announcements. On Windows the
	// field is ignored and execution falls back to the non-PTY
	// streaming path; file-level announcements still land, but
	// within-file bar updates don't. Skipped in guided mode.
	UsePTY bool `json:"-"`
}

// MarshalJSON renders Timeout as a duration string ("30s", "10m").
//
// time.Duration marshals to a raw nanosecond count by default, so the wire
// carried `"timeout": 10000000000` — a number that reads as ten billion
// seconds unless you already know the unit. The string carries its own
// unit, and matches how every other duration on this API is rendered.
//
// The coordinator decodes plans produced by a worker, so the two halves
// have to agree; UnmarshalJSON below is the other half.
func (s Step) MarshalJSON() ([]byte, error) {
	type stepFields Step // shed the methods, or this recurses
	timeout := ""
	if s.Timeout > 0 {
		timeout = s.Timeout.String()
	}
	// The outer Timeout shadows the embedded one: encoding/json resolves a
	// name collision in favour of the shallower field.
	return json.Marshal(struct {
		stepFields
		Timeout string `json:"timeout,omitempty"`
	}{stepFields(s), timeout})
}

// UnmarshalJSON accepts the duration string MarshalJSON emits, and a bare
// number of nanoseconds for anything that hand-builds a step body.
func (s *Step) UnmarshalJSON(data []byte) error {
	type stepFields Step
	aux := struct {
		*stepFields
		Timeout json.RawMessage `json:"timeout,omitempty"`
	}{stepFields: (*stepFields)(s)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if len(aux.Timeout) == 0 || string(aux.Timeout) == "null" {
		s.Timeout = 0
		return nil
	}
	var text string
	if err := json.Unmarshal(aux.Timeout, &text); err == nil {
		d, err := time.ParseDuration(text)
		if err != nil {
			return fmt.Errorf("step %d: invalid timeout %q: %w", s.Number, text, err)
		}
		s.Timeout = d
		return nil
	}
	var ns int64
	if err := json.Unmarshal(aux.Timeout, &ns); err != nil {
		return fmt.Errorf("step %d: timeout must be a duration string or nanosecond count", s.Number)
	}
	s.Timeout = time.Duration(ns)
	return nil
}

// StepVerify defines how to check that a step completed correctly.
type StepVerify struct {
	Toolkit              ToolkitSelection            `json:"-"`
	PreVerify            func(context.Context) error `json:"-"`
	ExecutionEnvironment []string                    `json:"-"`
	Environment          map[string]string           `json:"-"`
	Python               string                      `json:"python,omitempty"`
	RuntimeChecks        *schema.RuntimeChecks       `json:"runtime_checks,omitempty"`
	Type                 string                      `json:"type"` // "file_exists", "file_equals", "command_output", "dir_exists"
	Command              string                      `json:"command,omitempty"`
	Path                 string                      `json:"path,omitempty"`
	Expected             string                      `json:"expected,omitempty"`
	// Transient marks a check whose subject the install consumes — a
	// downloaded archive that extraction unpacks and cleanup removes.
	// It is meaningful while the step runs and meaningless afterwards,
	// so VerifyAll (which asks "is this install intact?") does not treat
	// its absence as damage. VerifyStep is unaffected: a step-by-step
	// caller asking "did the download land?" still gets the literal
	// answer.
	Transient bool `json:"transient,omitempty"`
	// PlanScoped marks a check that compares the tree against THIS plan's
	// parameters rather than against install integrity. The two questions
	// have different right answers: a plan preview asks "what would this
	// plan change?" and must report a version mismatch literally, while
	// install/verify asks "is this install intact?" and must not — a node
	// running a release other than the one the plan names has a working
	// install, and version drift is already reported through version_source.
	//
	// A missing or empty subject is damage under either question and is
	// never neutralized.
	PlanScoped bool `json:"plan_scoped,omitempty"`
}

// StepResult is the outcome of verifying a single step.
type StepResult struct {
	Checks []RuntimeCheck `json:"checks,omitempty"`
	Step   int            `json:"step"`
	Passed bool           `json:"passed"`
	Actual string         `json:"actual,omitempty"`
	// Unverifiable marks a step that declares no check at all, as
	// opposed to one whose check succeeded. Passed stays true because
	// nothing failed, but the two must not read the same: a step with
	// no Verify cannot attest anything, and reporting it as satisfied
	// is the false green this package exists to avoid. The checksum
	// step is the deliberate case — the comparison it performs leaves
	// no artifact to look for afterwards.
	Unverifiable bool   `json:"unverifiable,omitempty"`
	Message      string `json:"message"`
}

// VerifyResult is the outcome of verifying all steps.
type VerifyResult struct {
	CheckContract string         `json:"check_contract,omitempty"`
	Checks        []RuntimeCheck `json:"checks,omitempty"`
	Provider      string         `json:"provider"`
	Steps         []StepResult   `json:"steps"`
	AllOK         bool           `json:"all_ok"`
}

// Execute runs all steps in the plan automatically.
// Calls progress with human-readable messages after each step.
func (p *Plan) Execute(ctx context.Context, progress func(string)) error {
	for _, step := range p.Steps {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if progress != nil {
			progress(fmt.Sprintf("[%d/%d] %s", step.Number, len(p.Steps), step.Description))
		}

		if err := executeStep(ctx, step); err != nil {
			if step.Optional {
				slog.Warn("Optional step failed, continuing", "step", step.Number, "description", step.Description, "error", err)
			} else {
				return fmt.Errorf("step %d (%s) failed: %w", step.Number, step.Description, enrichLoadFailure(err))
			}
		}

		// Verify the step (skip for optional steps that already failed)
		if !step.Optional {
			result := VerifyStepContext(ctx, step)
			if !result.Passed {
				return fmt.Errorf("step %d verification failed: %s", step.Number, result.Message)
			}
		}
	}
	return nil
}

// ExecuteStep runs a single step by number, then verifies it.
//
// Unlike Execute (automated), guided single-step runs always return the real
// Verify result. Optional is a signal to Execute's batch loop that a failure
// should not abort the plan — it does NOT mean "report success regardless"
// in guided mode, where the user ran the step specifically to learn whether
// it worked.
func (p *Plan) ExecuteStep(ctx context.Context, stepNum int) *StepResult {
	for _, step := range p.Steps {
		if step.Number != stepNum {
			continue
		}
		if err := executeStep(ctx, step); err != nil {
			if step.Optional {
				slog.Warn("Optional step command failed, checking verify state", "step", step.Number, "error", err)
			} else {
				return &StepResult{
					Step:    step.Number,
					Passed:  false,
					Message: fmt.Sprintf("step failed: %v", err),
				}
			}
		}
		vr := VerifyStepContext(ctx, step)
		return &vr
	}
	return &StepResult{Step: stepNum, Passed: false, Message: fmt.Sprintf("step %d not found in plan", stepNum)}
}

// VerifyAll runs verification checks for all steps sequentially, asking
// "is this install intact?".
// A failed required step short-circuits subsequent steps and flips AllOK.
// Optional failures are recorded in StepResult but do not propagate to AllOK
// or abort downstream verification — Optional means "may legitimately fail."
//
// Use ProbeState instead to ask what a plan would change: the two questions
// disagree about a PlanScoped check, and only this one forgives a mismatch.
func (p *Plan) VerifyAll() *VerifyResult {
	return p.VerifyAllContext(context.Background())
}

// VerifyAllContext bounds prerequisite checks by the caller's lifetime.
func (p *Plan) VerifyAllContext(ctx context.Context) *VerifyResult {
	return p.verify(ctx, true)
}

// ProbeState reports each step's current on-disk state for a plan preview,
// answering "what would running this plan change?".
//
// It differs from VerifyAll on exactly one point: a PlanScoped mismatch is
// reported literally. A node holding a different release than this plan names
// has an intact install (so VerifyAll passes it) but running the plan WOULD
// change it, so a preview that called it installed would describe a reinstall
// as a no-op — which is how a downgrade became invisible.
func (p *Plan) ProbeState() []StepState {
	return p.ProbeStateContext(context.Background())
}

// ProbeStateContext bounds preview checks by the caller's lifetime.
func (p *Plan) ProbeStateContext(ctx context.Context) []StepState {
	return StatesFromVerify(p.verify(ctx, false).Steps)
}

// verify walks the steps once. forgivePlanScoped selects which of the two
// questions above is being asked.
func (p *Plan) verify(ctx context.Context, forgivePlanScoped bool) *VerifyResult {
	vr := &VerifyResult{
		Provider:      p.Provider,
		CheckContract: "install_steps_v1",
		Steps:         make([]StepResult, 0, len(p.Steps)),
		AllOK:         true,
	}

	failed := false
	for _, step := range p.Steps {

		if forgivePlanScoped && step.Verify.Transient && p.Recipe != nil {
			vr.Steps = append(vr.Steps, StepResult{Step: step.Number, Passed: true, Message: "candidate check; active runtime verified separately"})
			continue
		}
		if failed && step.Verify.Type != "runtime_checks" {
			vr.Steps = append(vr.Steps, StepResult{Step: step.Number, Passed: false, Message: "skipped: previous step not verified"})
			continue
		}
		result := VerifyStepContext(ctx, step)
		if step.Verify.Type == "runtime_checks" {
			vr.CheckContract = RuntimeCheckContract
			vr.Checks = append(vr.Checks, result.Checks...)
		}
		mismatch := planScopedMismatch(step.Verify, result)
		if forgivePlanScoped && mismatch {
			result.Passed = true
			result.Message = fmt.Sprintf("holds %q, not this plan's %q: a different release, not a damaged install",
				result.Actual, step.Verify.Expected)
		}
		if !result.Passed && step.Verify.Transient {
			// The artifact did its job and went away. Reporting that as
			// damage made every completed llama.cpp install verify false,
			// and the cascade below buried the steps that actually say
			// whether the install is intact.
			result.Passed = true
			result.Message = "transient artifact, consumed by a later step"
		}
		vr.Steps = append(vr.Steps, result)
		// A plan-scoped mismatch is a difference, not a failure. Letting it
		// open the cascade made every step after it read "skipped: previous
		// step not verified" with installed:false — ollama records its
		// version at step 9 of 11, so a node one release behind reported its
		// managed marker as absent while the marker sat on disk. The steps
		// after a version difference are still probeable and still true.
		if !result.Passed && !step.Optional && !mismatch {
			vr.AllOK = false
			failed = true
		}
	}
	return vr
}

// planScopedMismatch reports whether r failed only because its subject holds
// a value other than the one this plan names — as opposed to being missing or
// empty, which is damage either way. VerifyStep records what it read in
// Actual, so a non-empty Actual is what separates "different" from "absent".
//
// Restricted to file_equals deliberately. The Actual test only means "wrong
// value" for a check that cannot fail any other way with output in hand:
// command_output assigns Actual BEFORE it inspects the exit status, so a
// command that fails while printing anything would be forgiven here as a
// version difference — "holds \"sh: not found\", not this plan's \"b10549\"".
// A new type adopting PlanScoped must teach this function what its own
// mismatch looks like rather than inherit file_equals' shape.
func planScopedMismatch(v StepVerify, r StepResult) bool {
	return v.PlanScoped && v.Type == "file_equals" && !r.Passed && r.Actual != ""
}

// VerifyStep runs the verification check for a single step.
func VerifyStep(step Step) StepResult { return VerifyStepContext(context.Background(), step) }

// VerifyStepContext runs a check with the caller's cancellation boundary.
func VerifyStepContext(ctx context.Context, step Step) StepResult {
	result := StepResult{Step: step.Number}
	if step.Verify.PreVerify != nil {
		if err := step.Verify.PreVerify(ctx); err != nil {
			result.Message = err.Error()
			return result
		}
	}

	switch step.Verify.Type {
	case "runtime_checks":
		if step.Verify.RuntimeChecks == nil {
			result.Message = "runtime checks not declared"
			break
		}
		result.Checks = runRuntimeChecksSnapshot(ctx, step.Verify.Python, *step.Verify.RuntimeChecks, step.Verify.Environment, step.Verify.ExecutionEnvironment, step.Verify.Toolkit)
		result.Passed = true
		for _, check := range result.Checks {
			if !check.Passed {
				result.Passed = false
			}
		}
		result.Message = "runtime prerequisites checked"

	case "file_exists":
		if _, err := os.Stat(step.Verify.Path); err == nil {
			result.Passed = true
			result.Message = "file exists"
		} else {
			result.Message = fmt.Sprintf("file not found: %s", step.Verify.Path)
		}

	// file_equals answers "does this file hold the value the plan says it
	// should", where file_exists only answers "is something there". Used for
	// the version marker, so a plan for one release stops reporting itself
	// as already installed against a tree holding another.
	//
	// Equality, not substring: "b1045" is a prefix of "b10453", and a check
	// that a downgrade satisfies is the check this exists to replace. The
	// content is trimmed to match fsroot.ReadInstalledVersion, which is what
	// the rest of the system reads this file with.
	case "file_equals":
		// An empty Expected cannot mean anything, and it would otherwise
		// pass against an empty file — which IsInstalled reads as NOT
		// installed, so the two answers would contradict each other. No
		// installer builds one today; this keeps a future one from
		// producing a check that silently means nothing.
		if step.Verify.Expected == "" {
			result.Message = "file_equals has no expected value"
			break
		}
		data, err := os.ReadFile(step.Verify.Path)
		if err != nil {
			result.Message = fmt.Sprintf("file not found: %s", step.Verify.Path)
			break
		}
		actual := strings.TrimSpace(string(data))
		result.Actual = actual
		if actual == step.Verify.Expected {
			result.Passed = true
			result.Message = "file content matches"
		} else {
			result.Message = fmt.Sprintf("expected %q, found %q", step.Verify.Expected, actual)
		}

	case "dir_exists":
		info, err := os.Stat(step.Verify.Path)
		if err == nil && info.IsDir() {
			result.Passed = true
			result.Message = "directory exists"
		} else {
			result.Message = fmt.Sprintf("directory not found: %s", step.Verify.Path)
		}

	case "command_output":
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		cmd, cmdErr := shellCommand(ctx, step.Verify.Command)
		if cmdErr != nil {
			result.Message = fmt.Sprintf("command setup failed: %v", cmdErr)
			return result
		}
		output, err := cmd.CombinedOutput()
		actual := strings.TrimSpace(string(output))
		result.Actual = actual

		if err != nil {
			result.Message = fmt.Sprintf("command failed: %v", err)
		} else if step.Verify.Expected != "" && !strings.Contains(actual, step.Verify.Expected) {
			result.Message = fmt.Sprintf("expected %q in output, got %q", step.Verify.Expected, actual)
		} else {
			result.Passed = true
			result.Message = "command output matches"
		}

	default:
		result.Passed = true
		result.Unverifiable = true
		result.Message = "no verification configured"
	}

	return result
}

// shellCommand creates an exec.Cmd that runs a shell command string
// on the current platform: "sh -c" on Unix, `cmd /S /C "..."` on Windows.
//
// Windows note: naive `cmd /C command-with-quoted-paths` fails because
// cmd.exe has a peculiar "preserve quotes only if conditions met" rule
// that mangles paths like `mkdir "C:\Users\Foo\AppData\..."`. The
// documented workaround (see cmd.exe /? and Microsoft docs) is to pass
// /S /C together with the command wrapped in an outer pair of double
// quotes -- /S makes cmd strip exactly the outer pair and treat the
// rest verbatim. We also have to bypass Go's default argument quoting
// via syscall.SysProcAttr.CmdLine, because Go's `exec.Command` would
// re-escape the inner quotes with backslashes on Windows.
// Error return is reserved for future construction failures; today both
// platform builders succeed synchronously.
//
//nolint:unparam // error return is the signature contract, not dead code
func shellCommand(ctx context.Context, command string) (*exec.Cmd, error) {
	if runtime.GOOS == "windows" {
		return newWindowsShellCommand(ctx, command)
	}
	return host.CommandContext(ctx, "sh", "-c", command), nil
}

func executeStep(ctx context.Context, step Step) error {
	if step.Verify.Type == "runtime_checks" && step.PreExec == nil && step.PostExec == nil {
		result := VerifyStepContext(ctx, step)
		if !result.Passed {
			for _, check := range result.Checks {
				if !check.Passed {
					return fmt.Errorf("%s: %s (%s)", check.Name, check.Reason, check.Actual)
				}
			}
			return fmt.Errorf("runtime prerequisites failed: %s", result.Message)
		}
		return nil
	}
	// Per-step timeout
	if step.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, step.Timeout)
		defer cancel()
	}

	if step.ServiceAction != "" {
		return executeServiceAction(ctx, step.ServiceAction)
	}

	// Pre-exec hook (automated mode only — security checks, preflight)
	if step.PreExec != nil {
		if err := step.PreExec(ctx); err != nil {
			return fmt.Errorf("pre-exec: %w", err)
		}
	}

	// Execute shell command
	if step.Command != "" {
		cmd, cmdErr := shellCommand(ctx, step.Command)
		if cmdErr != nil {
			return fmt.Errorf("command setup: %w", cmdErr)
		}

		if step.WorkingDir != "" {
			cmd.Dir = step.WorkingDir
		}

		// Compose the subprocess env: parent env → install-scoped env
		// (node.yaml providers.proxy → HTTP_PROXY/SSL_CERT_FILE/etc.) →
		// per-step Env. Later writes win, so a plan can still override
		// anything the global env set. We only allocate when at least one
		// source contributes entries — otherwise cmd.Env stays nil and
		// os/exec inherits the parent env for free.
		globalEnv := fsroot.ExecEnv()
		if step.ExecutionEnvironment != nil {
			cmd.Env = append([]string{}, step.ExecutionEnvironment...)
		}
		if step.ExecutionEnvironment == nil && (len(globalEnv) > 0 || len(step.Env) > 0) {
			cmd.Env = append(cmd.Env, os.Environ()...)
			cmd.Env = append(cmd.Env, globalEnv...)
			for k, v := range step.Env {
				cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
			}
		}

		if step.Stdin != "" {
			cmd.Stdin = strings.NewReader(step.Stdin)
		}

		if step.StdoutLine != nil {
			if step.UsePTY {
				if err := runStepPTY(ctx, cmd, step.StdoutLine); err != nil {
					return err
				}
			} else if err := runStepStreaming(ctx, cmd, step.StdoutLine); err != nil {
				return err
			}
		} else {
			if err := runStepStreaming(ctx, cmd, nil); err != nil {
				return err
			}
		}
	}

	// Post-exec hook (automated mode only — checksum verification, symlink safety)
	if step.PostExec != nil {
		if err := step.PostExec(ctx); err != nil {
			return fmt.Errorf("post-exec: %w", err)
		}
	}

	return nil
}

// loadFailureExitCodes maps NTSTATUS / Unix loader exit codes that
// indicate the binary failed to start (vs. ran and exited non-zero) to
// human-readable hints. The values appear in `os/exec.ExitError`'s
// stringified form as "exit status 0xc0000005" on Windows or "exit
// status 127" on Unix.
//
// Centralized so adding a new platform's loader code is a one-liner.
var loadFailureExitCodes = map[string]string{
	// Windows: STATUS_DLL_NOT_FOUND. The PE loader couldn't resolve a
	// DLL listed in the import table — most often a runtime
	// redistributable not installed (cuBLAS, MSVC, etc).
	"exit status 0xc0000135": "binary failed to start: a required DLL is missing. " +
		"Check runtime dependencies of the binary; for CUDA variants, ensure the cudart " +
		"redistributable was extracted alongside the executable.",
	// Windows: STATUS_ACCESS_VIOLATION. Process started but crashed
	// during initialization. Common cause is a CUDA runtime mismatch
	// or partial DLL set (cudart present but cublas missing).
	"exit status 0xc0000005": "binary crashed during startup (ACCESS_VIOLATION). " +
		"Most often caused by a CUDA/runtime DLL mismatch; verify the cudart redistributable " +
		"matches the binary's CUDA version (e.g. cudart-llama-bin-win-cuda-13.1).",
	// Windows: STATUS_ENTRYPOINT_NOT_FOUND. A specific symbol was
	// missing — typically older runtime DLL than the binary needs.
	"exit status 0xc0000139": "binary failed to start: an expected symbol is missing from a runtime DLL. " +
		"The runtime redistributable is older than the binary expects; reinstall a matching runtime.",
}

// enrichLoadFailure inspects a step error and adds a hint when the
// exit code is a known load-failure pattern. If err is nil or the exit
// code isn't recognized, the original error is returned unchanged.
//
// Detection is string-based (matching the formatted suffix from
// os/exec.ExitError) because the underlying syscall-specific exit codes
// aren't portable to compare directly. Cheap and safe — false-positives
// require an exact match on the formatted prefix.
func enrichLoadFailure(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for code, hint := range loadFailureExitCodes {
		if strings.Contains(msg, code) {
			return fmt.Errorf("%w\n\nhint: %s", err, hint)
		}
	}
	return err
}
