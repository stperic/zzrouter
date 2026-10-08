// Package lint contains tests that enforce project coding standards.
// These run as part of `go test ./...` and catch convention violations before
// they land, independent of whichever version of golangci-lint the developer
// happens to have installed.
//
// To exempt a line, add a comment: // lint:allow <rule>
// Example: now := time.Now() // lint:allow time.Now
//
// To add a new rule, append to the rules slice in TestCodingStandards.
//
// Pattern borrowed from AlphaDB's internal/lint/standards_test.go.
package lint

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// rule defines a single coding standard check.
type rule struct {
	Name    string         // human-readable rule name
	Pattern *regexp.Regexp // regex to match violations
	Message string         // explanation shown on violation

	// Scope controls which files are scanned.
	// Empty IncludeDirs = all production Go files under the project root.
	IncludeDirs []string // only scan these dirs (relative to project root)
	ExcludeDirs []string // skip these dirs (always excludes _test.go, vendor)

	// AllowTag is the `lint:allow` tag that exempts a line. Defaults to rule Name.
	AllowTag string
}

func (r rule) tag() string {
	if r.AllowTag != "" {
		return r.AllowTag
	}
	return r.Name
}

// violation records a single rule failure.
type violation struct {
	File    string
	Line    int
	Content string
	Rule    string
	Message string
}

func (v violation) String() string {
	return fmt.Sprintf("%s:%d: [%s] %s\n  > %s", v.File, v.Line, v.Rule, v.Message, strings.TrimSpace(v.Content))
}

func TestCodingStandards(t *testing.T) {
	projectRoot := findProjectRoot(t)

	// Most rules scope to the same Phase-1 server-side directory set.
	// Phase 2 will widen the scope when TUI / CLI client migrates.
	serverScope := []string{
		"pkg",
		"internal/server",
		"internal/cli/servercli",
		"cmd/zzrouter-node",
		"cmd/zzrouter-launcher",
	}

	rules := []rule{
		{
			Name:    "time.Now",
			Pattern: regexp.MustCompile(`\btime\.Now\(\)`),
			Message: "Use utils.Now() for duration math (preserves monotonic) or utils.NowUTC() " +
				"for wire/persistence (strips monotonic, UTC). Bare time.Now() is a footgun: " +
				"wrapping it in .UTC() silently drops the monotonic reading and turns every " +
				"time.Since into wall-clock subtraction, breaking circuit breakers, rate " +
				"limiters, and any TTL that must survive an NTP step. See pkg/utils/time.go. " +
				"pkg/utils/time.go is the sole implementation site and carries lint:allow tags.",
			IncludeDirs: serverScope,
		},
		{
			// Writing utils.Now().UTC() defeats the whole point of the Now /
			// NowUTC split: .UTC() strips the monotonic reading that Now()
			// was carefully preserving. Whoever types this wanted UTC — the
			// correct spelling is utils.NowUTC(), which goes through the same
			// injectable Clock and normalises at one place.
			Name:    "utils.Now-UTC-chain",
			Pattern: regexp.MustCompile(`\butils\.Now\(\)\.UTC\(\)`),
			Message: "Use utils.NowUTC() instead of utils.Now().UTC(). Chaining .UTC() onto " +
				"Now() strips the monotonic reading that Now() preserved for duration math; " +
				"NowUTC() is the correct single-call entry point for persistence/wire output. " +
				"See pkg/utils/time.go.",
			IncludeDirs: serverScope,
		},
		{
			// ioutil has been deprecated since Go 1.16 (Feb 2021). Every
			// function is a thin alias for one in io or os. New code should
			// never import it; existing imports are a yellow flag that the
			// file hasn't been touched in four years and may carry other
			// stale patterns.
			Name:    "ioutil",
			Pattern: regexp.MustCompile(`\bioutil\.`),
			Message: "ioutil is deprecated since Go 1.16. Use os.ReadFile, os.WriteFile, " +
				"io.ReadAll, io.Discard, or os.CreateTemp directly — ioutil functions are " +
				"thin aliases for these and produce deprecation warnings under staticcheck.",
			IncludeDirs: serverScope,
		},
		{
			// context.TODO() is the stdlib marker for "I need a Context but
			// don't have one yet." Shipping it to production means the call
			// graph above the TODO was never wired to propagate context, so
			// any upstream cancellation / deadline is silently dropped. In a
			// request-driven server that usually translates to goroutine
			// leaks when the client disconnects mid-stream.
			Name:    "context.TODO",
			Pattern: regexp.MustCompile(`\bcontext\.TODO\(\)`),
			Message: "context.TODO() signals unfinished work. Use the request context (propagate " +
				"c.Request.Context() from Gin handlers, or ctx from the caller) or, when there " +
				"is truly no caller context (background loop entrypoint), use context.Background() " +
				"with a comment explaining why cancellation does not apply.",
			IncludeDirs: serverScope,
		},
		{
			// http.DefaultClient and http.DefaultTransport are package-global
			// singletons with NO request timeout. In a distributed system
			// that talks to LLM providers, cluster peers, and registries,
			// using them is an uncapped-blocking-I/O bug waiting to fire:
			// one slow upstream stalls the goroutine forever, accumulates
			// file descriptors, and eventually exhausts the listener.
			// Construct a local &http.Client{Timeout: X} (or wrap the
			// transport) so the timeout is visible and scoped.
			Name:    "http.Default-client",
			Pattern: regexp.MustCompile(`\bhttp\.(DefaultClient|DefaultTransport)\b`),
			Message: "http.DefaultClient / http.DefaultTransport have no timeout — a slow upstream " +
				"stalls the caller forever and leaks the goroutine. Construct a local " +
				"&http.Client{Timeout: X} with an explicit deadline. See pkg/prov_apps/install/" +
				"download.go and llamacpp.go for the project pattern.",
			IncludeDirs: serverScope,
		},
		{
			// fmt.Errorf("...: %v", err) stringifies the error instead of
			// wrapping it. Callers lose the ability to errors.Is / errors.As
			// the original sentinel — the chain is severed the moment the
			// wrapped error is formatted. .golangci.yml already bans string
			// matching on err.Error() (see forbidigo rules); this closes the
			// loop by ensuring the error graph is preserved at the wrap site.
			//
			// The pattern requires `err)` immediately after the verb — that
			// anchors the check to the canonical Go-idiom wrap form and
			// avoids false positives on identifiers that merely contain
			// "err" as a substring (e.g. errMsg, errCh).
			Name:    "errorf-v-wrap",
			Pattern: regexp.MustCompile(`fmt\.Errorf\("[^"]*: %v",\s*err\)`),
			Message: "Use %w instead of %v when wrapping an error: fmt.Errorf(\"...: %w\", err). " +
				"%v stringifies the error and severs the chain, so errors.Is / errors.As " +
				"cannot reach the original sentinel. The project already bans string-matching " +
				"on err.Error() (.golangci.yml) — preserving the chain at the wrap site is " +
				"the other half of that contract.",
			IncludeDirs: serverScope,
		},
		{
			// http.NewRequest uses context.Background() internally — the
			// constructed request cannot be cancelled by the caller. In a
			// proxy / cluster coordinator that means: a client disconnects
			// mid-stream, but the outbound request to the upstream LLM
			// keeps going. Goroutine + socket leak per disconnect. Use
			// http.NewRequestWithContext(ctx, ...) with a context from
			// either c.Request.Context() (Gin handlers), the caller's ctx
			// parameter, or a bounded context.WithTimeout for fire-and-
			// forget work.
			//
			// staticcheck has no rule for this; contextcheck checks
			// propagation but does not flag the NewRequest vs
			// NewRequestWithContext choice at construction.
			Name:    "http.NewRequest-ctx",
			Pattern: regexp.MustCompile(`\bhttp\.NewRequest\(`),
			Message: "Use http.NewRequestWithContext(ctx, ...) instead of http.NewRequest. " +
				"NewRequest internally uses context.Background(), so the request cannot be " +
				"cancelled when the caller's context is done — a client disconnecting leaves " +
				"the outbound request and its goroutine running until the upstream responds. " +
				"Pass c.Request.Context() in Gin handlers, the caller's ctx parameter in " +
				"service methods, or context.WithTimeout(context.Background(), ...) for " +
				"fire-and-forget worker goroutines.",
			IncludeDirs: serverScope,
		},
		{
			// Coord endpoints fanning out to peers MUST go through the
			// established cluster mTLS proxy (s.cluster.router or the
			// JobsProxy mTLS pattern). Building a raw http://host:port
			// URL targets the worker's admin port (9090, narrowed to
			// 127.0.0.1 since 2026-04-28) and "connection refused" is
			// the failure mode at runtime. The lint catches the most
			// common anti-pattern: Sprintf-ing an http URL with a
			// hostname placeholder. See feedback memory
			// `feedback_no_direct_worker_reach`.
			Name: "direct-peer-http",
			// Match Sprintf-style URL construction that targets a
			// peer's zzrouter admin path. Backend-instance proxying
			// (raw provider port, no /zzrouter prefix) is a separate
			// concern handled by proxyToInstance and not in scope.
			Pattern: regexp.MustCompile(`Sprintf\("https?://%s[^"]*/zzrouter/`),
			Message: "Direct HTTP construction targeting a peer's zzrouter admin endpoint is " +
				"forbidden. Use s.cluster.router.Unicast (mTLS via cluster port) or the " +
				"JobsProxy mTLS pattern in jobs_proxy.go. The worker admin port (9090) is " +
				"narrowed to 127.0.0.1 — direct reaches \"connection refused\" at runtime. " +
				"See feedback memory `feedback_no_direct_worker_reach` and pkg/cluster/router.",
			IncludeDirs: []string{"internal/server"},
		},
		{
			// log.Fatal / log.Fatalf / log.Fatalln call os.Exit(1) under the
			// hood; log.Panic* call panic after logging. Either one from
			// library code bypasses graceful shutdown, deferred cleanup,
			// and any caller-level error handling. In a server process
			// that owns TCP listeners, background reapers, and pending
			// cluster state, an Exit from deep in the call stack leaves
			// half-committed files and open connections. Return an error
			// and let main() (or the test binary) decide whether to exit.
			Name:    "log.Fatal-Panic",
			Pattern: regexp.MustCompile(`\blog\.(Fatal|Panic)`),
			Message: "log.Fatal* calls os.Exit and log.Panic* calls panic — both bypass " +
				"graceful shutdown and deferred cleanup. Library code must return an error " +
				"and let the caller decide; only main() may terminate the process (and even " +
				"there, prefer slog + os.Exit with a documented exit code).",
			IncludeDirs: serverScope,
		},
		{
			// md5, sha1, des, rc4 are broken or weak primitives. Gosec's
			// G501/G505 catch this too, but the linter config on this box
			// is currently a version mismatch (see README on v2 migration),
			// so a duplicate guardrail in plain `go test` is worth the
			// three regex characters.
			//
			// md5/sha1 are still acceptable for non-security uses
			// (ETag, cache key digests); if you need that, prefer
			// hash/fnv or hash/crc32 and leave a comment explaining why
			// the weaker primitive would have been wrong anyway.
			Name:    "weak-crypto",
			Pattern: regexp.MustCompile(`"crypto/(md5|sha1|des|rc4)"`),
			Message: "crypto/md5, crypto/sha1, crypto/des, crypto/rc4 are broken or weak. " +
				"Use crypto/sha256 / crypto/sha512 / crypto/aes / crypto/chacha20poly1305. " +
				"For non-security hashing (cache keys, ETags) prefer hash/fnv or hash/crc32.",
			IncludeDirs: serverScope,
		},
		{
			// runtime.GC and runtime.GOMAXPROCS are almost always wrong in
			// application code. Manual GC is cargo-culted from benchmarking
			// contexts; GOMAXPROCS has been auto-tuned by the runtime
			// since Go 1.5 and container-aware via automaxprocs in cases
			// where the scheduler gets confused. runtime.SetFinalizer is
			// a footgun — finalizers may run late, never, or after the
			// object's references have been collected; explicit Close /
			// context lifecycle is always clearer.
			Name:    "runtime-manual",
			Pattern: regexp.MustCompile(`\bruntime\.(GC|GOMAXPROCS|SetFinalizer)\(`),
			Message: "runtime.GC / runtime.GOMAXPROCS / runtime.SetFinalizer are rarely correct " +
				"in application code. Manual GC masks real allocation problems; GOMAXPROCS is " +
				"the runtime's job (use go.uber.org/automaxprocs if container awareness is " +
				"needed); finalizers are unreliable — prefer explicit Close / context cancellation.",
			IncludeDirs: serverScope,
		},
		{
			// os.Setenv mutates process-wide state. In a server it races
			// with concurrent readers (anything calling os.Getenv from
			// another goroutine), and in tests it breaks t.Parallel()
			// between any two tests that touch overlapping vars. The
			// legitimate production use cases in zzrouter are confined
			// to pkg/config/dotenv.go, which loads user-provided .env
			// files — those two call sites carry a lint:allow tag.
			// Everything else must go through pkg/config, which reads
			// from the env once at startup and freezes it into config
			// structs.
			Name:    "os.Setenv",
			Pattern: regexp.MustCompile(`\bos\.Setenv\(`),
			Message: "os.Setenv mutates process-wide state and races with concurrent os.Getenv. " +
				"Read env vars once at startup through pkg/config and pass the resolved value " +
				"into the code that needs it. The only legitimate setters are in pkg/config/" +
				"dotenv.go (loading user .env files) and they carry a lint:allow tag.",
			IncludeDirs: serverScope,
		},
		{
			// Provider-params error codes (plan §7.2) live in
			// pkg/httperr/param_code.go as a closed enum. Any call site
			// writing the literal string drifts silently from the enum —
			// the constant can be renamed in Go, the wire string cannot.
			Name:    "param-error-code-literal",
			Pattern: regexp.MustCompile(`"code"\s*:\s*"(unknown_flag|wrong_type|out_of_range|unknown_node|unknown_model|coercion_failed|retired)"`),
			Message: "Use the ParamErrorCode constants from pkg/httperr " +
				"(CodeUnknownFlag, CodeWrongType, CodeOutOfRange, CodeUnknownNode, " +
				"CodeUnknownModel, CodeCoercionFailed, CodeRetired) and the " +
				"utils.ParamError type inside the problem envelope. String " +
				"literals of these codes at call sites drift away from the enum " +
				"when wire values change.",
			IncludeDirs: serverScope,
			ExcludeDirs: []string{"pkg/httperr"},
		},
	}

	for _, r := range rules {
		r := r
		t.Run(r.Name, func(t *testing.T) {
			violations := scanForViolations(t, projectRoot, r)
			for _, v := range violations {
				t.Errorf("\n%s", v)
			}
			if len(violations) > 0 {
				t.Logf("%d violation(s) found. To exempt a specific line, append: // lint:allow %s",
					len(violations), r.tag())
			}
		})
	}
}

func scanForViolations(t *testing.T, root string, r rule) []violation {
	t.Helper()
	var violations []violation

	// Build the set of directories to walk.
	var walkRoots []string
	if len(r.IncludeDirs) > 0 {
		for _, d := range r.IncludeDirs {
			walkRoots = append(walkRoots, filepath.Join(root, d))
		}
	} else {
		walkRoots = []string{root}
	}

	excludeSet := make(map[string]bool)
	for _, d := range r.ExcludeDirs {
		excludeSet[d] = true
	}

	for _, walkRoot := range walkRoots {
		err := filepath.Walk(walkRoot, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				// A path this guard cannot reach is not a path it has cleared, so it
				// is reported rather than skipped: a silent skip lets the standard
				// break in exactly the file nobody can read.
				violations = append(violations, violation{
					File: path, Rule: r.tag(), Message: "not walkable: " + err.Error(),
				})
				return nil //nolint:nilerr // recorded as a violation; keep walking to report every one
			}
			if info.IsDir() {
				name := info.Name()
				if name == "vendor" || name == "mocks" || name == ".git" || name == "testdata" {
					return filepath.SkipDir
				}
				rel, _ := filepath.Rel(root, path)
				if excludeSet[rel] {
					return filepath.SkipDir
				}
				return nil
			}

			// Only scan production Go files.
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}

			violations = append(violations, scanFile(path, r)...)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", walkRoot, err)
		}
	}

	return violations
}

func scanFile(path string, r rule) []violation {
	f, err := os.Open(path)
	if err != nil {
		return []violation{{File: path, Rule: r.tag(), Message: "unreadable: " + err.Error()}}
	}
	defer f.Close()

	allowTag := "lint:allow " + r.tag()

	var violations []violation
	scanner := bufio.NewScanner(f)
	// Raise the buffer: some files have long generated SQL literals.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for lineNum := 1; scanner.Scan(); lineNum++ {
		line := scanner.Text()
		if strings.Contains(line, allowTag) {
			continue
		}
		if !r.Pattern.MatchString(line) {
			continue
		}
		violations = append(violations, violation{
			File:    path,
			Line:    lineNum,
			Content: line,
			Rule:    r.Name,
			Message: r.Message,
		})
	}
	return violations
}

// findProjectRoot walks up from the test file to find the go.mod.
func findProjectRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find project root (go.mod)")
		}
		dir = parent
	}
}
