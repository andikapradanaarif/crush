package agent

import (
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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			admitted, decisions := selectOpenFailures(tc.prompt, tc.failures, "")
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
		[]cmdlog.Failure{fresh}, dir)
	require.Len(t, admitted, 1)

	// File touched after last_seen → stale_suspect.
	stale := fresh
	stale.LastSeen = time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(decoyFile, time.Now(), time.Now()))
	_, decisions := selectOpenFailures(
		"fix the failing test in decoy/decoy_test.go",
		[]cmdlog.Failure{stale}, dir)
	require.Equal(t, failStaleSuspect, decisions[0].Reason)

	// Every implicated file absent → path_gone.
	gone := mkFailure("go test ./decoy", ".", "decoy/deleted_test.go")
	_, decisions = selectOpenFailures(
		"fix the failing test in decoy/deleted_test.go",
		[]cmdlog.Failure{gone}, dir)
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
		[]cmdlog.Failure{mkFailure("go test ./v1.0", ".")}, dir)
	require.Len(t, admitted, 1)

	_, decisions := selectOpenFailures("fix v2.0/handler.go",
		[]cmdlog.Failure{mkFailure("go test ./v1.0", ".")}, dir)
	require.Equal(t, failReferentNone, decisions[0].Reason,
		"a dotted dir absent from disk reads as a URL, not scope")

	// A bare dir name mints scope when it exists on disk — "fix decoy"
	// carries no path signal of its own.
	admitted, _ = selectOpenFailures("fix decoy",
		[]cmdlog.Failure{mkFailure("go test ./decoy", ".")}, dir)
	require.Len(t, admitted, 1)

	// And the same bare word mints negative scope under a cue.
	_, decisions = selectOpenFailures("fix main.go — don't touch decoy",
		[]cmdlog.Failure{mkFailure("go test ./decoy", ".")}, dir)
	require.Equal(t, failNegatedScope, decisions[0].Reason)
}
