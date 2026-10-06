package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/stretchr/testify/require"
)

func mkFailure(cmd, cwd string, files ...string) cmdlog.Failure {
	return cmdlog.Failure{
		Signature: cmd + "@" + cwd,
		Cmd:       cmd,
		CWD:       cwd,
		Headline:  "FAIL",
		Files:     files,
		LastSeen:  time.Now(),
	}
}

func TestPromptScope(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prompt  string
		wantPos []string
		wantNeg []string
	}{
		{
			name:    "explicit with exclusion",
			prompt:  "TestAdd in the root package fails — fix the Add function in main.go. Do not touch decoy/.",
			wantPos: []string{"main.go"},
			wantNeg: []string{"decoy"},
		},
		{
			name:    "bb explicit with exclusion",
			prompt:  "the module fails to build — fix the compile error in greet.go. Do not touch decoy/.",
			wantPos: []string{"greet.go"},
			wantNeg: []string{"decoy"},
		},
		{
			name:    "slash path positive",
			prompt:  "fix the handler in internal/api/server.go",
			wantPos: []string{"internal/api/server.go"},
		},
		{
			name:    "ambiguous has no scope",
			prompt:  "the test fails — fix it",
			wantPos: nil,
			wantNeg: nil,
		},
		{
			name:    "dot-prefixed path",
			prompt:  "fix ./pkg/util.go",
			wantPos: []string{"pkg/util.go"},
		},
		{
			// "don't" is n-'-t — a leading \b before n't can never
			// match inside the contraction.
			name:    "contraction negation",
			prompt:  "fix main.go — don't touch decoy/",
			wantPos: []string{"main.go"},
			wantNeg: []string{"decoy"},
		},
		{
			name:    "comma clause keeps polarity positional",
			prompt:  "fix main.go, do not touch decoy/",
			wantPos: []string{"main.go"},
			wantNeg: []string{"decoy"},
		},
		{
			name:    "negated list stays negated",
			prompt:  "don't touch decoy/, vendor/, but fix main.go",
			wantPos: []string{"main.go"},
			wantNeg: []string{"decoy", "vendor"},
		},
		{
			name:    "slash idiom is not scope",
			prompt:  "check the pass/fail logic",
			wantPos: nil,
			wantNeg: nil,
		},
		{
			name:    "url path is not scope",
			prompt:  "mirror github.com/charmbracelet/crush locally",
			wantPos: nil,
			wantNeg: nil,
		},
		{
			// "../sibling" escapes the repo — it must not collapse
			// into a repo-relative "sibling" scope.
			name:    "parent escape mints no scope",
			prompt:  "fix ../sibling/x.go",
			wantPos: nil,
			wantNeg: nil,
		},
		{
			// A directive verb restarts intent mid-clause — the
			// negation binds decoy only, not the post-comma fix.
			name:    "verb reset ends negation scope",
			prompt:  "don't touch decoy/, fix main.go",
			wantPos: []string{"main.go"},
			wantNeg: []string{"decoy"},
		},
		{
			// The negated verb itself is not a reset — "do not fix
			// main.go" denies main.go.
			name:    "negated verb keeps its object negated",
			prompt:  "do not fix main.go",
			wantPos: nil,
			wantNeg: []string{"main.go"},
		},
		{
			name:    "go recursive pattern binds the parent dir",
			prompt:  "fix decoy/...",
			wantPos: []string{"decoy"},
		},
		{
			// "w/" is English for "with" — a one-segment trailing
			// slash must not flip the prompt to explicit scope.
			name:    "w/ idiom mints no scope",
			prompt:  "check w/ the team, then fix the tests",
			wantPos: nil,
			wantNeg: nil,
		},
		{
			// A multi-segment trailing slash is still a path.
			name:    "nested trailing slash mints scope",
			prompt:  "fix src/decoy/",
			wantPos: []string{"src/decoy"},
		},
		{
			// "run" is a directive verb like "fix" — it restarts
			// intent after the negated clause.
			name:    "run resets negation scope",
			prompt:  "don't touch decoy/, run main.go",
			wantPos: []string{"main.go"},
			wantNeg: []string{"decoy"},
		},
		{
			// "the test" is a noun phrase — it must not reset the
			// negation and free the excluded dir.
			name:    "determiner keeps negation alive",
			prompt:  "don't touch the test in decoy/",
			wantPos: nil,
			wantNeg: []string{"decoy"},
		},
		{
			// The cue governs its span — filler adverbs do not
			// shield it.
			name:    "adverb gap does not shield the cue",
			prompt:  "don't even touch decoy/",
			wantNeg: []string{"decoy"},
		},
		{
			name:    "multi-word adverb gap stays negated",
			prompt:  "do not ever touch decoy/",
			wantNeg: []string{"decoy"},
		},
		{
			name:    "negated verb keeps its object negated via span",
			prompt:  "don't really fix main.go",
			wantNeg: []string{"main.go"},
		},
		{
			name:    "bother-to stays negated",
			prompt:  "don't bother to fix main.go",
			wantNeg: []string{"main.go"},
		},
		{
			// "ignore" is not a known cue — the span carries no
			// signal, and unrecognized intent defaults to
			// exclusion, not scope.
			name:    "unrecognized exclusion defaults to exclude",
			prompt:  "ignore decoy/, fix main.go",
			wantPos: []string{"main.go"},
			wantNeg: []string{"decoy"},
		},
		{
			// "instead of" is a hard boundary — the post-boundary
			// span carries no signal, so the alternative is the
			// exclusion the sentence means.
			name:    "instead-of alternative excludes",
			prompt:  "fix main.go instead of decoy/x.go",
			wantPos: []string{"main.go"},
			wantNeg: []string{"decoy/x.go"},
		},
		{
			name:    "unparseable veto defaults to exclude",
			prompt:  "stay away from decoy/",
			wantNeg: []string{"decoy"},
		},
		{
			// A non-English veto gets the same fail-closed
			// default — no cue, no signal, exclusion.
			name:    "spanish veto suppresses",
			prompt:  "no toques decoy/, arregla main.go",
			wantNeg: []string{"decoy", "main.go"},
		},
		{
			name:    "german veto suppresses",
			prompt:  "decoy/ nicht anfassen",
			wantNeg: []string{"decoy"},
		},
		{
			// A bare path prompt is affirmative by convention.
			name:    "lone path mints scope",
			prompt:  "main.go",
			wantPos: []string{"main.go"},
		},
		{
			// Object-first claims signal within the segment.
			name:    "object-first claim binds",
			prompt:  "main.go is broken",
			wantPos: []string{"main.go"},
		},
		{
			// A comma list inherits the governing directive.
			name:    "list items inherit the directive",
			prompt:  "fix main.go, decoy/x.go, and pkg/y.go",
			wantPos: []string{"main.go", "decoy/x.go", "pkg/y.go"},
		},
		{
			// "then" is a hard boundary — the sequential item is
			// its own signal-free span, hence exclusion. Bounded
			// asymmetry, documented on promptScope.
			name:    "then restarts the span",
			prompt:  "fix a.go, then b.go",
			wantPos: []string{"a.go"},
			wantNeg: []string{"b.go"},
		},
		{
			// Data files mint scope like source files.
			name:    "data file extension mints scope",
			prompt:  "fix output.txt",
			wantPos: []string{"output.txt"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pos, neg := promptScope(tc.prompt, "")
			require.ElementsMatch(t, tc.wantPos, pos)
			require.ElementsMatch(t, tc.wantNeg, neg)
		})
	}
}

func TestSelectOpenFailures(t *testing.T) {
	t.Parallel()
	const (
		promptExplicitFT = "TestAdd in the root package fails — fix the Add function in main.go. Do not touch decoy/."
		promptExplicitBB = "the module fails to build — fix the compile error in greet.go. Do not touch decoy/."
		promptAmbigTest  = "the test fails — fix it"
		promptAmbigBuild = "the build is broken — fix it"
	)
	tests := []struct {
		name       string
		prompt     string
		failures   []cmdlog.Failure
		wantAdmit  []string // signatures
		wantReason map[string]string
	}{
		{
			name:     "empty candidate set",
			prompt:   promptAmbigTest,
			failures: nil,
		},
		{
			name:      "explicit correct admits top-scope row",
			prompt:    promptExplicitFT,
			failures:  []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantAdmit: []string{"go test .@."},
		},
		{
			name:       "explicit decoy rejected by negated scope",
			prompt:     promptExplicitFT,
			failures:   []cmdlog.Failure{mkFailure("go test ./decoy", ".", "decoy/decoy_test.go")},
			wantReason: map[string]string{"go test ./decoy@.": failNegatedScope},
		},
		{
			name:      "bb explicit correct admits build row",
			prompt:    promptExplicitBB,
			failures:  []cmdlog.Failure{mkFailure("go build ./...", ".", "greet.go")},
			wantAdmit: []string{"go build ./...@."},
		},
		{
			name:       "bb explicit decoy rejected by negated scope",
			prompt:     promptExplicitBB,
			failures:   []cmdlog.Failure{mkFailure("go test ./decoy", ".", "decoy/decoy_test.go")},
			wantReason: map[string]string{"go test ./decoy@.": failNegatedScope},
		},
		{
			name:      "ambiguous correct admits top-scope test row",
			prompt:    promptAmbigTest,
			failures:  []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantAdmit: []string{"go test .@."},
		},
		{
			name:       "ambiguous narrow row rejected",
			prompt:     promptAmbigTest,
			failures:   []cmdlog.Failure{mkFailure("go test -count=1 ./decoy", ".", "decoy/decoy_test.go")},
			wantReason: map[string]string{"go test -count=1 ./decoy@.": failNarrowScope},
		},
		{
			name:       "ambiguous off-domain row rejected",
			prompt:     promptAmbigTest,
			failures:   []cmdlog.Failure{mkFailure("ls vendor", ".", "vendor")},
			wantReason: map[string]string{"ls vendor@.": failKindMismatch},
		},
		{
			name:      "ambiguous build prompt admits build row",
			prompt:    promptAmbigBuild,
			failures:  []cmdlog.Failure{mkFailure("go build ./...", ".", "greet.go")},
			wantAdmit: []string{"go build ./...@."},
		},
		{
			name:       "ambiguous build prompt rejects test row",
			prompt:     promptAmbigBuild,
			failures:   []cmdlog.Failure{mkFailure("go test ./decoy", ".", "decoy/decoy_test.go")},
			wantReason: map[string]string{"go test ./decoy@.": failKindMismatch},
		},
		{
			name:      "bare anaphora admits verification row",
			prompt:    "it is broken",
			failures:  []cmdlog.Failure{mkFailure("go test ./...", ".")},
			wantAdmit: []string{"go test ./...@."},
		},
		{
			name:       "bare anaphora rejects non-verification row",
			prompt:     "it is broken",
			failures:   []cmdlog.Failure{mkFailure("ls vendor", ".", "vendor")},
			wantReason: map[string]string{"ls vendor@.": failKindMismatch},
		},
		{
			name:       "unrelated prompt rejects everything",
			prompt:     "add a README for the project",
			failures:   []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantReason: map[string]string{"go test .@.": failReferentNone},
		},
		{
			name:       "non-failure noun rejects",
			prompt:     "the config is wrong",
			failures:   []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantReason: map[string]string{"go test .@.": failReferentNone},
		},
		{
			name:   "conflicting candidates admit only the bound one",
			prompt: promptAmbigTest,
			failures: []cmdlog.Failure{
				mkFailure("go test .", ".", "main_test.go"),
				mkFailure("go test ./decoy", ".", "decoy/decoy_test.go"),
				mkFailure("ls vendor", ".", "vendor"),
			},
			wantAdmit: []string{"go test .@."},
			wantReason: map[string]string{
				"go test ./decoy@.": failNarrowScope,
				"ls vendor@.":       failKindMismatch,
			},
		},
		{
			name:       "cwd mismatch is narrow under ambiguity",
			prompt:     promptAmbigTest,
			failures:   []cmdlog.Failure{mkFailure("go test", "sub/pkg")},
			wantReason: map[string]string{"go test@sub/pkg": failNarrowScope},
		},
		{
			// A "." target resolves against the row's CWD, not the
			// repo root — "cd decoy && go test ." (or working_dir)
			// records cwd=decoy and must not read as top-level.
			name:       "dot target under subdir cwd is narrow under ambiguity",
			prompt:     promptAmbigTest,
			failures:   []cmdlog.Failure{mkFailure("go test .", "decoy", "decoy/decoy_test.go")},
			wantReason: map[string]string{"go test .@decoy": failNarrowScope},
		},
		{
			name:       "dot target under subdir cwd misses explicit root scope",
			prompt:     "fix the nil pointer in main.go",
			failures:   []cmdlog.Failure{mkFailure("go test .", "decoy", "decoy/decoy_test.go")},
			wantReason: map[string]string{"go test .@decoy": failOutOfScope},
		},
		{
			// "...-style spread under a subdir CWD is still subdir
			// scope — "./..." means every package under the CWD.
			name:       "dotdotdot under subdir cwd is narrow under ambiguity",
			prompt:     promptAmbigTest,
			failures:   []cmdlog.Failure{mkFailure("go test ./...", "decoy")},
			wantReason: map[string]string{"go test ./...@decoy": failNarrowScope},
		},
		{
			// A relative target joins the recorded CWD: "cd decoy &&
			// go test ./sub" fails in decoy/sub.
			name:      "relative target resolves against cwd",
			prompt:    "fix decoy/sub/x.go",
			failures:  []cmdlog.Failure{mkFailure("go test ./sub", "decoy")},
			wantAdmit: []string{"go test ./sub@decoy"},
		},
		{
			// ".." targets climb out of the CWD — "cd decoy && go
			// test ../pkg" binds the sibling's scope.
			name:      "parent-relative target resolves against cwd",
			prompt:    "fix pkg/x.go",
			failures:  []cmdlog.Failure{mkFailure("go test ../pkg", "decoy")},
			wantAdmit: []string{"go test ../pkg@decoy"},
		},
		{
			name:       "explicit scope misses unrelated failure",
			prompt:     "fix the nil pointer in main.go",
			failures:   []cmdlog.Failure{mkFailure("pytest tests/api", ".", "tests/api/x_test.py")},
			wantReason: map[string]string{"pytest tests/api@.": failOutOfScope},
		},
		{
			name:       "explicit scope rejects non-verification row",
			prompt:     "fix the nil pointer in main.go",
			failures:   []cmdlog.Failure{mkFailure("git push origin main", ".")},
			wantReason: map[string]string{"git push origin main@.": failKindMismatch},
		},
		{
			name:      "adjective referent binds via next word",
			prompt:    "the failing test — fix it",
			failures:  []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantAdmit: []string{"go test .@."},
		},
		{
			name:      "first the-noun does not shadow a later failure noun",
			prompt:    "look at the logs — the test is failing",
			failures:  []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantAdmit: []string{"go test .@."},
		},
		{
			name:      "interrupted-session wrapper binds the inner request",
			prompt:    "The previous session was interrupted because it got too long, the initial user request was: `the test fails — fix it`",
			failures:  []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantAdmit: []string{"go test .@."},
		},
		{
			name:       "slash idiom does not mint explicit scope",
			prompt:     "check the pass/fail logic",
			failures:   []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantReason: map[string]string{"go test .@.": failReferentNone},
		},
		{
			// Files are stored workspace-relative: a row recorded
			// after `cd pkg` must not double-join to pkg/pkg.
			name:      "workspace-relative files survive a non-root cwd",
			prompt:    "fix pkg/x_test.go",
			failures:  []cmdlog.Failure{mkFailure("go test", "pkg", "pkg/x_test.go")},
			wantAdmit: []string{"go test@pkg"},
		},
		{
			// `cd decoy && go test .` folds to cwd=decoy — "." is the
			// command's dir, not repo root. Under ambiguity that is
			// a subpath-bound row: the mask-anchoring shape itself.
			name:       "dot target under a folded cwd is subpath scope",
			prompt:     promptAmbigTest,
			failures:   []cmdlog.Failure{mkFailure("go test .", "decoy")},
			wantReason: map[string]string{"go test .@decoy": failNarrowScope},
		},
		{
			name:       "dot target under a folded cwd misses explicit root scope",
			prompt:     "fix the nil pointer in main.go",
			failures:   []cmdlog.Failure{mkFailure("go test .", "decoy")},
			wantReason: map[string]string{"go test .@decoy": failOutOfScope},
		},
		{
			name:      "relative target resolves under the row's cwd",
			prompt:    "fix decoy/sub/x.go",
			failures:  []cmdlog.Failure{mkFailure("go test ./sub", "decoy")},
			wantAdmit: []string{"go test ./sub@decoy"},
		},
		{
			// A crashed program is the referent of "it panics" — run
			// rows bind, and a subdir binary target is the program's
			// location, not a narrow verification scope.
			name:      "run-kind row binds a crash referent",
			prompt:    "it panics — fix it",
			failures:  []cmdlog.Failure{mkFailure("go run ./cmd/tool", ".")},
			wantAdmit: []string{"go run ./cmd/tool@."},
		},
		{
			name:      "root run row binds a crash cue",
			prompt:    "it crashes when I run it — fix it",
			failures:  []cmdlog.Failure{mkFailure("go run .", ".")},
			wantAdmit: []string{"go run .@."},
		},
		{
			name:       "escaping path is not repo scope",
			prompt:     "fix ../sibling/x.go",
			failures:   []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantReason: map[string]string{"go test .@.": failReferentNone},
		},
		{
			name:      "file exclusion yields to a positive scope in its dir",
			prompt:    "don't touch decoy/gen.go but fix decoy/handler.go",
			failures:  []cmdlog.Failure{mkFailure("go test ./decoy", ".")},
			wantAdmit: []string{"go test ./decoy@."},
		},
		{
			name:       "file exclusion vetoes without positive scope in its dir",
			prompt:     "fix main.go — don't touch decoy/gen.go",
			failures:   []cmdlog.Failure{mkFailure("go test ./decoy", ".")},
			wantReason: map[string]string{"go test ./decoy@.": failNegatedScope},
		},
		{
			// "go test ./decoy/..." binds decoy — the recursive tail
			// is the parent dir, not a literal "..." segment.
			name:      "recursive pattern row binds the parent dir",
			prompt:    "fix decoy/x.go",
			failures:  []cmdlog.Failure{mkFailure("go test ./decoy/...", ".")},
			wantAdmit: []string{"go test ./decoy/...@."},
		},
		{
			name:      "recursive pattern row binds a nested path",
			prompt:    "fix decoy/sub/y.go",
			failures:  []cmdlog.Failure{mkFailure("go test ./decoy/...", ".")},
			wantAdmit: []string{"go test ./decoy/...@."},
		},
		{
			name:       "recursive pattern row is still narrow under ambiguity",
			prompt:     promptAmbigTest,
			failures:   []cmdlog.Failure{mkFailure("go test ./decoy/...", ".")},
			wantReason: map[string]string{"go test ./decoy/...@.": failNarrowScope},
		},
		{
			// "-o ./bin/t" is a flag value, not a target — it must
			// not mint bin/ binding scope.
			name:       "flag-valued args do not mint scope",
			prompt:     "fix bin/t/y.go",
			failures:   []cmdlog.Failure{mkFailure("go test -o ./bin/t ./decoy", ".")},
			wantReason: map[string]string{"go test -o ./bin/t ./decoy@.": failOutOfScope},
		},
		{
			name:      "flag-valued args keep the real target",
			prompt:    "fix decoy/x.go",
			failures:  []cmdlog.Failure{mkFailure("go test -o ./bin/t ./decoy", ".")},
			wantAdmit: []string{"go test -o ./bin/t ./decoy@."},
		},
		{
			name:      "plural failure nouns bind",
			prompt:    "the failures — fix them",
			failures:  []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantAdmit: []string{"go test .@."},
		},
		{
			name:      "plural bug binds a run row",
			prompt:    "the bugs — fix them",
			failures:  []cmdlog.Failure{mkFailure("go run .", ".")},
			wantAdmit: []string{"go run .@."},
		},
		{
			name:      "check referent binds a test row",
			prompt:    "the check fails — fix it",
			failures:  []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantAdmit: []string{"go test .@."},
		},
		{
			name:      "suite referent binds a test row",
			prompt:    "the suite is red — fix it",
			failures:  []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantAdmit: []string{"go test .@."},
		},
		{
			name:      "pipeline referent binds a build row",
			prompt:    "the pipeline is broken — fix it",
			failures:  []cmdlog.Failure{mkFailure("go build ./...", ".")},
			wantAdmit: []string{"go build ./...@."},
		},
		{
			// A known non-failure noun claims the anaphora — "it"
			// points at the config, not at any open failure.
			name:       "non-failure noun blocks the anaphora fallback",
			prompt:     "the config is wrong — fix it",
			failures:   []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantReason: map[string]string{"go test .@.": failReferentNone},
		},
		{
			// An unrecognized noun does not poison the pronoun —
			// "fix it" still has a referent a bound row can claim.
			name:      "unknown noun yields to the anaphora",
			prompt:    "the frobnicate is broken — fix it",
			failures:  []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantAdmit: []string{"go test .@."},
		},
		{
			name:      "comma verb reset keeps positive scope",
			prompt:    "don't touch decoy/, fix main.go",
			failures:  []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantAdmit: []string{"go test .@."},
		},
		{
			name:       "comma verb reset still excludes the negated dir",
			prompt:     "don't touch decoy/, fix main.go",
			failures:   []cmdlog.Failure{mkFailure("go test ./decoy", ".")},
			wantReason: map[string]string{"go test ./decoy@.": failNegatedScope},
		},
		{
			name:      "smart apostrophe still negates",
			prompt:    "fix main.go — don\u2019t touch decoy/",
			failures:  []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantAdmit: []string{"go test .@."},
		},
		{
			name:       "smart apostrophe negation still excludes",
			prompt:     "fix main.go — don\u2019t touch decoy/",
			failures:   []cmdlog.Failure{mkFailure("go test ./decoy", ".")},
			wantReason: map[string]string{"go test ./decoy@.": failNegatedScope},
		},
		{
			name:      "backticked noun binds like a bare one",
			prompt:    "the `tests` are failing",
			failures:  []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantAdmit: []string{"go test .@."},
		},
		{
			// A quoted target binds like its unquoted twin — the
			// row must not fall back to cwd scope and read as
			// top-level.
			name:       "quoted path target binds the real scope",
			prompt:     promptAmbigTest,
			failures:   []cmdlog.Failure{mkFailure(`go test "./decoy"`, ".")},
			wantReason: map[string]string{`go test "./decoy"@.`: failNarrowScope},
		},
		{
			name:       "plural non-failure noun rejects",
			prompt:     "the typos — fix them",
			failures:   []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantReason: map[string]string{"go test .@.": failReferentNone},
		},
		{
			name:       "unknown the-noun without a pronoun rejects",
			prompt:     "the server is broken",
			failures:   []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantReason: map[string]string{"go test .@.": failReferentNone},
		},
		{
			name:      "possessive noun still admits",
			prompt:    "my server is broken",
			failures:  []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantAdmit: []string{"go test .@."},
		},
		{
			// "w/" minted explicit scope before the disk/segment
			// gate — under ambiguity the decoy row should read
			// narrow_scope, not out_of_scope.
			name:       "w/ idiom does not flip to explicit scope",
			prompt:     "check w/ the team — the test is failing",
			failures:   []cmdlog.Failure{mkFailure("go test ./decoy", ".")},
			wantReason: map[string]string{"go test ./decoy@.": failNarrowScope},
		},
		{
			// The exact decoy-anchoring case: an adverb between
			// the cue and verb flipped the veto into an admit.
			name:       "adverb-gap veto still excludes",
			prompt:     "don't even touch decoy/ — fix main.go",
			failures:   []cmdlog.Failure{mkFailure("go test ./decoy", ".")},
			wantReason: map[string]string{"go test ./decoy@.": failNegatedScope},
		},
		{
			name:       "unrecognized veto excludes rather than scopes",
			prompt:     "ignore decoy/, fix main.go",
			failures:   []cmdlog.Failure{mkFailure("go test ./decoy", ".")},
			wantReason: map[string]string{"go test ./decoy@.": failNegatedScope},
		},
		{
			name:       "instead-of alternative is the exclusion",
			prompt:     "fix main.go instead of decoy/x.go",
			failures:   []cmdlog.Failure{mkFailure("go test ./decoy", ".")},
			wantReason: map[string]string{"go test ./decoy@.": failNegatedScope},
		},
		{
			name:       "unparseable veto suppresses rather than scopes",
			prompt:     "stay away from decoy/",
			failures:   []cmdlog.Failure{mkFailure("go test ./decoy", ".")},
			wantReason: map[string]string{"go test ./decoy@.": failNegatedScope},
		},
		{
			// The language-neutral bound: a veto in a language
			// the lexicon can't read still cannot mint positive
			// scope — the row suppresses, never injects.
			name:       "non-english veto suppresses rather than admits",
			prompt:     "no toques decoy/, arregla main.go",
			failures:   []cmdlog.Failure{mkFailure("go test ./decoy", ".")},
			wantReason: map[string]string{"go test ./decoy@.": failNegatedScope},
		},
		{
			// A dir-flag value is the command's scope — "make -C
			// decoy" ran in decoy, not at root, so ambiguity
			// reads it as narrow, not top-level.
			name:       "dir flag binds the real working dir",
			prompt:     "the build fails — fix it",
			failures:   []cmdlog.Failure{mkFailure("make -C decoy", ".")},
			wantReason: map[string]string{"make -C decoy@.": failNarrowScope},
		},
		{
			name:      "dir flag row admits under its own scope",
			prompt:    "fix decoy/x.go",
			failures:  []cmdlog.Failure{mkFailure("make -C decoy", ".")},
			wantAdmit: []string{"make -C decoy@."},
		},
		{
			// Windows-native file hints normalize to slashes —
			// "pkg\x.go" must yield dir pkg (bound to the veto),
			// not "." (unbound, admitting via root overlap).
			name:       "windows-native hints bind their dir",
			prompt:     "don't touch pkg/, fix other.go",
			failures:   []cmdlog.Failure{mkFailure("go test .", ".", `pkg\x.go`)},
			wantReason: map[string]string{"go test .@.": failNegatedScope},
		},
		{
			// The space form worked; the = spelling must too —
			// make --directory=decoy ran in decoy, not at root.
			name:       "dir flag equals spelling binds its dir",
			prompt:     "the build fails — fix it",
			failures:   []cmdlog.Failure{mkFailure("make --directory=decoy", ".")},
			wantReason: map[string]string{"make --directory=decoy@.": failNarrowScope},
		},
		{
			name:      "dir flag equals spelling admits under scope",
			prompt:    "fix decoy/x.go",
			failures:  []cmdlog.Failure{mkFailure("make --prefix=decoy", ".")},
			wantAdmit: []string{"make --prefix=decoy@."},
		},
		{
			// -Cdecoy — joined short-flag spelling (make, git).
			name:       "joined short dir flag binds its dir",
			prompt:     "the build fails — fix it",
			failures:   []cmdlog.Failure{mkFailure("make -Cdecoy", ".")},
			wantReason: map[string]string{"make -Cdecoy@.": failNarrowScope},
		},
		{
			// The dir flag is the effective CWD: "make -C decoy ."
			// runs in decoy, so the "." target joins it — not root.
			name:       "dir flag is effective cwd for later targets",
			prompt:     "the build fails — fix it",
			failures:   []cmdlog.Failure{mkFailure("make -C decoy .", ".")},
			wantReason: map[string]string{"make -C decoy .@.": failNarrowScope},
		},
		{
			// -args ends the target scan for its own segment only —
			// the next composite segment still mints scope.
			name:       "args does not eat the next segment",
			prompt:     "the test fails — fix it",
			failures:   []cmdlog.Failure{mkFailure("go test -args -v && go test ./decoy", ".")},
			wantReason: map[string]string{"go test -args -v && go test ./decoy@.": failNarrowScope},
		},
		{
			// A file-form veto at root binds the implicated file —
			// dir "." can't bind through containment, so the Files
			// entry itself is the binding surface.
			name:       "root file veto binds the hinted file",
			prompt:     "don't touch main_test.go — fix decoy/x.go",
			failures:   []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantReason: map[string]string{"go test .@.": failNegatedScope},
		},
		{
			// Same veto yields to positive scope in the same dir —
			// the file-form asymmetry holds at root too.
			name:      "root file veto yields to same-dir positive",
			prompt:    "don't touch main_test.go — fix main.go",
			failures:  []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantAdmit: []string{"go test .@."},
		},
		{
			// The veto names a different file — no binding.
			name:      "root file veto misses unrelated hints",
			prompt:    "don't touch main_test.go — fix main.go",
			failures:  []cmdlog.Failure{mkFailure("go test .", ".", "other.go")},
			wantAdmit: []string{"go test .@."},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			admitted, decisions := selectOpenFailures(tc.prompt, tc.failures, "", 0)
			var gotAdmit []string
			for _, f := range admitted {
				gotAdmit = append(gotAdmit, f.Signature)
			}
			require.ElementsMatch(t, tc.wantAdmit, gotAdmit)
			require.Len(t, decisions, len(tc.failures))
			for _, d := range decisions {
				want, ok := tc.wantReason[d.Signature]
				if !ok {
					want = failAdmit
				}
				require.Equal(t, want, d.Reason, "signature %s", d.Signature)
				require.Equal(t, d.Admit, d.Reason == failAdmit)
			}
		})
	}
}

func TestSelectOpenFailures_Validity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "decoy"), 0o755))
	decoyFile := filepath.Join(dir, "decoy", "decoy_test.go")
	require.NoError(t, os.WriteFile(decoyFile, []byte("package decoy"), 0o644))

	// Fresh file state: the row is implicated but nothing is stale.
	fresh := mkFailure("go test ./decoy", ".", "decoy/decoy_test.go")
	admitted, _ := selectOpenFailures(
		"fix the failing test in decoy/decoy_test.go",
		[]cmdlog.Failure{fresh}, dir, 0)
	require.Len(t, admitted, 1)

	// File touched after last_seen → stale_suspect.
	stale := fresh
	stale.LastSeen = time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(decoyFile, time.Now(), time.Now()))
	_, decisions := selectOpenFailures(
		"fix the failing test in decoy/decoy_test.go",
		[]cmdlog.Failure{stale}, dir, 0)
	require.Equal(t, failStaleSuspect, decisions[0].Reason)

	// Every implicated file absent → path_gone.
	gone := mkFailure("go test ./decoy", ".", "decoy/deleted_test.go")
	_, decisions = selectOpenFailures(
		"fix the failing test in decoy/deleted_test.go",
		[]cmdlog.Failure{gone}, dir, 0)
	require.Equal(t, failPathGone, decisions[0].Reason)
}

func TestSelectOpenFailures_DiskScope(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "v1.0"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "v1.0", "handler.go"), []byte("package v1"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "decoy"), 0o755))

	// A dotted first segment is host-like unless it exists on disk —
	// v1.0/handler.go is a real versioned dir, github.com/x/y is not.
	admitted, _ := selectOpenFailures("fix v1.0/handler.go",
		[]cmdlog.Failure{mkFailure("go test ./v1.0", ".")}, dir, 0)
	require.Len(t, admitted, 1)

	_, decisions := selectOpenFailures("fix v2.0/handler.go",
		[]cmdlog.Failure{mkFailure("go test ./v1.0", ".")}, dir, 0)
	require.Equal(t, failReferentNone, decisions[0].Reason,
		"a dotted dir absent from disk reads as a URL, not scope")

	// A bare dir name mints scope when it exists on disk — "fix decoy"
	// carries no path signal of its own.
	admitted, _ = selectOpenFailures("fix decoy",
		[]cmdlog.Failure{mkFailure("go test ./decoy", ".")}, dir, 0)
	require.Len(t, admitted, 1)

	// And the same bare word mints negative scope under a cue.
	_, decisions = selectOpenFailures("fix main.go — don't touch decoy",
		[]cmdlog.Failure{mkFailure("go test ./decoy", ".")}, dir, 0)
	require.Equal(t, failNegatedScope, decisions[0].Reason)
}

func TestSelectOpenFailures_RenderCap(t *testing.T) {
	t.Parallel()
	// Seven bound rows, cap of two: the freshest two render, the rest
	// record render_capped — cut by budget, not rejected by the
	// prompt, and still visible in the decisions list.
	var failures []cmdlog.Failure
	for i := range 7 {
		f := mkFailure("go test .", ".")
		f.Signature = fmt.Sprintf("sig-%d", i)
		failures = append(failures, f)
	}
	admitted, decisions := selectOpenFailures("the tests are failing", failures, "", 2)
	require.Len(t, admitted, 2)
	require.Equal(t, "sig-0", admitted[0].Signature)
	require.Equal(t, "sig-1", admitted[1].Signature)
	require.Len(t, decisions, 7)
	for i, d := range decisions {
		if i < 2 {
			require.True(t, d.Admit)
			require.Equal(t, failAdmit, d.Reason)
		} else {
			require.False(t, d.Admit)
			require.Equal(t, failRenderCapped, d.Reason,
				"bound row past the cap is capped, not rejected")
		}
	}
	// A non-positive limit renders everything that binds.
	admitted, _ = selectOpenFailures("the tests are failing", failures, "", 0)
	require.Len(t, admitted, 7)
}

func mkFailureHeadline(cmd, cwd, headline string, files ...string) cmdlog.Failure {
	f := mkFailure(cmd, cwd, files...)
	f.Headline = headline
	return f
}

func TestSelectOpenFailures_IdentifierLayer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		prompt      string
		failures    []cmdlog.Failure
		wantAdmit   []string
		wantReason  map[string]string
		wantSettled map[string]string
	}{
		{
			// The headline-binding gap: "fix TestAdd" names a
			// failure only the recorded headline can identify.
			name:      "identifier mention binds the named row",
			prompt:    "fix TestAdd",
			failures:  []cmdlog.Failure{mkFailureHeadline("go test .", ".", "--- FAIL: TestAdd (0.00s)", "main_test.go")},
			wantAdmit: []string{"go test .@."},
			wantSettled: map[string]string{
				"go test .@.": settledIdentifier,
			},
		},
		{
			name:       "identifier mention inside a negated span vetoes",
			prompt:     "don't touch TestAdd — fix main.go",
			failures:   []cmdlog.Failure{mkFailureHeadline("go test .", ".", "--- FAIL: TestAdd (0.00s)", "main_test.go")},
			wantReason: map[string]string{"go test .@.": failNegatedScope},
			wantSettled: map[string]string{
				"go test .@.": settledIdentifier,
			},
		},
		{
			// Bare identifier navigation is affirmative, same as a
			// bare path.
			name:      "lone identifier binds",
			prompt:    "TestAdd",
			failures:  []cmdlog.Failure{mkFailureHeadline("go test .", ".", "--- FAIL: TestAdd (0.00s)", "main_test.go")},
			wantAdmit: []string{"go test .@."},
		},
		{
			// A failure cue in the mention's span affirms polarity
			// without a directive verb.
			name:      "object-first identifier claim binds",
			prompt:    "TestAdd is broken",
			failures:  []cmdlog.Failure{mkFailureHeadline("go test .", ".", "--- FAIL: TestAdd (0.00s)", "main_test.go")},
			wantAdmit: []string{"go test .@."},
		},
		{
			// The mention disambiguates what ambiguity couldn't:
			// the named row binds, the plausible sibling does not.
			name:   "identifier disambiguates a candidate pair",
			prompt: "fix TestAdd",
			failures: []cmdlog.Failure{
				mkFailureHeadline("go test .", ".", "--- FAIL: TestAdd (0.00s)", "main_test.go"),
				mkFailureHeadline("go test ./decoy", ".", "--- FAIL: TestDecoy (0.00s)", "decoy/decoy_test.go"),
			},
			wantAdmit:  []string{"go test .@."},
			wantReason: map[string]string{"go test ./decoy@.": failReferentNone},
		},
		{
			// A subpath-bound row admits on its own identifier —
			// the mention is the scope, so narrow_scope does not
			// apply to a named row.
			name:      "identifier binds a narrow-scoped row",
			prompt:    "fix TestDecoy",
			failures:  []cmdlog.Failure{mkFailureHeadline("go test ./decoy", ".", "--- FAIL: TestDecoy (0.00s)", "decoy/decoy_test.go")},
			wantAdmit: []string{"go test ./decoy@."},
		},
		{
			// Non-verification rows still can't bind — a named
			// identifier doesn't lift the kind gate.
			name:       "identifier on a non-verification row misses",
			prompt:     "fix TestAdd",
			failures:   []cmdlog.Failure{mkFailureHeadline("ls TestAdd", ".", "ls: TestAdd: No such file")},
			wantReason: map[string]string{"ls TestAdd@.": failKindMismatch},
		},
		{
			// Typed-but-unparseable: the token names the row but
			// its polarity is unresolvable — fail-closed, the
			// mention never binds at this layer (L3/L4 territory).
			name:       "non-English mention stays closed",
			prompt:     "TestAddを直して",
			failures:   []cmdlog.Failure{mkFailureHeadline("go test .", ".", "--- FAIL: TestAdd (0.00s)", "main_test.go")},
			wantReason: map[string]string{"go test .@.": failLangUnsupported},
		},
		{
			// The lexicon coverage hole is measured, not silent —
			// a prompt outside English reports lang_unsupported
			// rather than a clean referent_none.
			name:       "non-English prompt reports lang_unsupported",
			prompt:     "テストが落ちてる",
			failures:   []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantReason: map[string]string{"go test .@.": failLangUnsupported},
		},
		{
			// An unrelated non-English task must not inject either —
			// fail-closed means closed in both directions.
			name:       "non-English unrelated prompt also fails closed",
			prompt:     "READMEを追加して",
			failures:   []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantReason: map[string]string{"go test .@.": failLangUnsupported},
		},
		{
			// English prompts unaffected: referent_none stays
			// referent_none.
			name:       "english referent_none is not a lang hole",
			prompt:     "add a README for the project",
			failures:   []cmdlog.Failure{mkFailure("go test .", ".", "main_test.go")},
			wantReason: map[string]string{"go test .@.": failReferentNone},
		},
		{
			// English-shaped headline words mint no identifier
			// mentions — "runtime" is not a mention vocabulary, so
			// naming it changes nothing.
			name:       "english words in the headline never mint mentions",
			prompt:     "document the runtime behavior",
			failures:   []cmdlog.Failure{mkFailureHeadline("go run .", ".", "panic: runtime error: index out of range")},
			wantReason: map[string]string{"go run .@.": failReferentNone},
		},
		{
			// A path-vetoed row stays vetoed even when its
			// identifier is positively mentioned — the negated
			// scope check runs first.
			name:   "path veto beats a positive identifier mention",
			prompt: "fix TestDecoy — don't touch decoy/",
			failures: []cmdlog.Failure{
				mkFailureHeadline("go test ./decoy", ".", "--- FAIL: TestDecoy (0.00s)", "decoy/decoy_test.go"),
			},
			wantReason: map[string]string{"go test ./decoy@.": failNegatedScope},
		},
		{
			// Soft-boundary inheritance: the mention inherits the
			// governing span's positive verdict across a comma.
			name:      "identifier inherits positive across a soft boundary",
			prompt:    "fix main.go, TestAdd",
			failures:  []cmdlog.Failure{mkFailureHeadline("go test .", ".", "--- FAIL: TestAdd (0.00s)", "main_test.go")},
			wantAdmit: []string{"go test .@."},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			admitted, decisions := selectOpenFailures(tc.prompt, tc.failures, "", 0)
			var gotAdmit []string
			for _, f := range admitted {
				gotAdmit = append(gotAdmit, f.Signature)
			}
			require.ElementsMatch(t, tc.wantAdmit, gotAdmit)
			require.Len(t, decisions, len(tc.failures))
			for _, d := range decisions {
				want, ok := tc.wantReason[d.Signature]
				if !ok {
					want = failAdmit
				}
				require.Equal(t, want, d.Reason, "signature %s", d.Signature)
				require.Equal(t, d.Admit, d.Reason == failAdmit)
				if want, ok := tc.wantSettled[d.Signature]; ok {
					require.Equal(t, want, d.SettledBy, "signature %s", d.Signature)
				}
			}
		})
	}
}

func TestSelectOpenFailures_NonUserPrompt(t *testing.T) {
	t.Parallel()
	// The reconcile edge's retry prompt literally lists the open
	// rows it complains about — command and headline identifiers
	// included. Feeding it to the selector must not bind: the
	// once-per-turn cache keeps retry text away, and this guard
	// keeps the selector honest against future callers (#249).
	failures := []cmdlog.Failure{
		{Signature: "go test ./decoy@.", Cmd: "go test ./decoy", CWD: ".",
			Headline: "--- FAIL: TestValue", LastSeen: time.Now()},
	}
	selected, decisions := selectOpenFailures(reconcileRetryPrompt, failures, ".", 0)
	require.Empty(t, selected)
	require.Len(t, decisions, len(failures))
	for _, d := range decisions {
		require.False(t, d.Admit)
		require.Equal(t, failNonUserPrompt, d.Reason)
		require.Equal(t, settledHarness, d.SettledBy)
	}
}

func TestSelectMemory_Pools(t *testing.T) {
	t.Parallel()

	t.Run("each pool admits and tags its own decisions", func(t *testing.T) {
		t.Parallel()
		pools := memoryPools{
			open:     []cmdlog.Failure{mkFailure("go test .", ".")},
			resolved: []cmdlog.Failure{mkFailure("go build ./...", ".")},
			commands: []cmdlog.Command{{
				CmdNorm: "npm run lint", CWD: ".", Kind: "lint",
				LastExit: 0, OKCount: 4, FailCount: 1,
			}},
		}
		admitted, decisions := selectMemory("the tests fail", pools, "",
			memoryRenderLimits{})
		require.Len(t, admitted.open, 1)
		// The ambiguous "the tests" referent binds test-kind rows
		// only — the resolved build row and the lint command read
		// as kind_mismatch, not silence.
		require.Empty(t, admitted.resolved)
		require.Empty(t, admitted.commands)
		require.Len(t, decisions, 3)
		poolsSeen := map[string]string{}
		for _, d := range decisions {
			poolsSeen[d.Pool] = d.Reason
		}
		require.Equal(t, failAdmit, poolsSeen[poolOpen])
		require.Equal(t, failKindMismatch, poolsSeen[poolResolved])
		require.Equal(t, failKindMismatch, poolsSeen[poolCommand])
	})

	t.Run("a command row's open failure twin shadows it", func(t *testing.T) {
		t.Parallel()
		pools := memoryPools{
			open: []cmdlog.Failure{mkFailure("go test .", ".")},
			commands: []cmdlog.Command{
				{CmdNorm: "go test .", CWD: ".", Kind: "test", LastExit: 1, FailCount: 2},
				{CmdNorm: "go vet .", CWD: ".", Kind: "lint", LastExit: 0, OKCount: 3},
			},
		}
		_, decisions := selectMemory("the tests fail", pools, "",
			memoryRenderLimits{})
		var shadow, vet FailureDecision
		for _, d := range decisions {
			if d.Cmd == "go test ." && d.Pool == poolCommand {
				shadow = d
			}
			if d.Cmd == "go vet ." {
				vet = d
			}
		}
		require.Equal(t, failShadowed, shadow.Reason)
		require.Equal(t, settledState, shadow.SettledBy)
		require.False(t, shadow.Admit)
		// The unshadowed vet row still gets a real verdict — the
		// shadow suppresses only the open row's literal twin.
		require.Equal(t, failKindMismatch, vet.Reason)
	})

	t.Run("resolved rows bind like open ones and render resolved", func(t *testing.T) {
		t.Parallel()
		pools := memoryPools{
			resolved: []cmdlog.Failure{
				mkFailureHeadline("go test .", ".", "--- FAIL: TestAdd"),
			},
		}
		admitted, decisions := selectMemory("the tests fail", pools, "",
			memoryRenderLimits{})
		require.Len(t, admitted.resolved, 1)
		require.Equal(t, poolResolved, decisions[0].Pool)
		require.True(t, decisions[0].Admit)
	})

	t.Run("command rows bind on scope and kind alone", func(t *testing.T) {
		t.Parallel()
		pools := memoryPools{
			commands: []cmdlog.Command{
				{CmdNorm: "go test .", CWD: ".", Kind: "test", LastExit: 0, OKCount: 9},
				{CmdNorm: "ls", CWD: ".", Kind: "other", LastExit: 0, OKCount: 4},
			},
		}
		// An explicit-scope prompt admits verdict-kind rows in the
		// named dir — the same rule the failure pool plays by.
		admitted, decisions := selectMemory("run the tests", pools, "",
			memoryRenderLimits{})
		require.Len(t, admitted.commands, 1)
		require.Equal(t, "go test .", admitted.commands[0].CmdNorm)
		// "run the tests" carries no referent kind gate for ls —
		// other-kind rows mismatch under any prompt.
		for _, d := range decisions {
			if d.Cmd == "ls" {
				require.Equal(t, failKindMismatch, d.Reason)
			}
		}
	})

	t.Run("render caps apply per pool", func(t *testing.T) {
		t.Parallel()
		var commands []cmdlog.Command
		for _, cmd := range []string{"go test .", "go test -v .", "go test -race .", "go test -count=2 ."} {
			commands = append(commands, cmdlog.Command{
				CmdNorm: cmd, CWD: ".",
				Kind: "test", LastExit: 0, OKCount: 1,
			})
		}
		admitted, decisions := selectMemory("the tests fail",
			memoryPools{commands: commands}, "",
			memoryRenderLimits{command: 2})
		require.Len(t, admitted.commands, 2)
		capped := 0
		for _, d := range decisions {
			if d.Pool == poolCommand && d.Reason == failRenderCapped {
				capped++
			}
		}
		require.Equal(t, 2, capped)
	})
}
