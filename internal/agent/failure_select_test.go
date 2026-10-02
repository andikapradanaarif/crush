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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pos, neg := promptScope(tc.prompt)
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
			name:       "explicit scope misses unrelated failure",
			prompt:     "fix the nil pointer in main.go",
			failures:   []cmdlog.Failure{mkFailure("pytest tests/api", ".", "tests/api/x_test.py")},
			wantReason: map[string]string{"pytest tests/api@.": failOutOfScope},
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
