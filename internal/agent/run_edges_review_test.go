package agent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// writeResult builds a clean-stopping run whose only mutating call is
// a bash redirect — the completion claim the fresh-review edge scans.
func writeResult() *fantasy.AgentResult {
	return &fantasy.AgentResult{Steps: []fantasy.StepResult{
		stepWith(fantasy.FinishReasonToolCalls,
			fantasy.ToolCallContent{
				ToolCallID: "tc-b", ToolName: "bash",
				Input: `{"command":"cat > f.go <<'EOF'\npackage main\nEOF"}`,
			},
			fantasy.ToolResultContent{
				ToolCallID: "tc-b", ToolName: "bash",
				Result: fantasy.ToolResultOutputContentText{Text: ""},
			},
		),
		stepWith(fantasy.FinishReasonStop, fantasy.TextContent{Text: "done"}),
	}}
}

// reviewAgent wires a test agent with the fresh-review arm on and a
// spy model in the session's large slot — the reviewer.
func reviewAgent(t *testing.T, spy *advisorSpyModel) (*sessionAgent, message.Service, string) {
	t.Helper()
	a, svc, sessionID := newGateTestAgent(t, &config.Config{})
	a.freshReview = true
	a.models = func(string) ModelSet { return ModelSet{Large: Model{Model: spy}} }
	return a, svc, sessionID
}

func TestFreshReviewScan(t *testing.T) {
	t.Parallel()

	t.Run("fires on a clean stop over writes when armed", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		a.freshReview = true
		tr := a.scanFreshReviewEdge(t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: writeResult()})
		require.NotNil(t, tr)
		require.True(t, tr.fire)
		require.NotEmpty(t, tr.writeSet)
	})

	t.Run("gated when the arm is off", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		tr := a.scanFreshReviewEdge(t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: writeResult()})
		require.NotNil(t, tr)
		require.Equal(t, edgeOutcomeGated, tr.hint)
	})

	t.Run("no writes means no completion risk to review", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		a.freshReview = true
		tr := a.scanFreshReviewEdge(t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: cleanResult()})
		require.Nil(t, tr)
	})

	t.Run("non-clean stops and subagents never review", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		a.freshReview = true
		// A stalled run is not a completion claim.
		tr := a.scanFreshReviewEdge(t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: writeResult(), stalled: true})
		require.Nil(t, tr)
		a.isSubAgent = true
		tr = a.scanFreshReviewEdge(t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: writeResult()})
		require.Nil(t, tr)
	})
}

func TestFreshReviewVerdicts(t *testing.T) {
	t.Parallel()

	t.Run("VERDICT ISSUES queues a fingerprinted repair", func(t *testing.T) {
		t.Parallel()
		spy := &advisorSpyModel{text: "VERDICT: ISSUES\nf.go — missing error return — the task asked for error handling"}
		a, svc, sessionID := reviewAgent(t, spy)
		_, err := svc.Create(t.Context(), sessionID, message.CreateMessageParams{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: "add error handling to f.go"}},
		})
		require.NoError(t, err)

		queued := runEdgesForTest(a, t.Context(),
			SessionAgentCall{SessionID: sessionID, RunID: "run-1"},
			edgeInput{result: writeResult(), currentAssistant: &message.Message{Role: message.Assistant}})
		require.True(t, queued)
		require.Equal(t, 1, spy.calls)
		// The bundle carries task + file list — never the transcript.
		require.Contains(t, spy.prompt, "add error handling to f.go")
		require.Contains(t, spy.prompt, "f.go")

		q, _ := a.messageQueue.Get(sessionID)
		require.Len(t, q, 1)
		require.True(t, strings.HasPrefix(q[0].Prompt, freshReviewPrefix))
		require.Contains(t, q[0].Prompt, "missing error return")
		require.True(t, IsRepairPrompt(q[0].Prompt))
	})

	t.Run("VERDICT PASS completes unreviewed", func(t *testing.T) {
		t.Parallel()
		spy := &advisorSpyModel{text: "VERDICT: PASS"}
		a, _, sessionID := reviewAgent(t, spy)
		queued := runEdgesForTest(a, t.Context(),
			SessionAgentCall{SessionID: sessionID},
			edgeInput{result: writeResult(), currentAssistant: &message.Message{Role: message.Assistant}})
		require.False(t, queued)
		require.Equal(t, 1, spy.calls)
	})

	t.Run("model failure degrades, never blocks", func(t *testing.T) {
		t.Parallel()
		spy := &advisorSpyModel{err: errors.New("provider down")}
		a, _, sessionID := reviewAgent(t, spy)
		queued := runEdgesForTest(a, t.Context(),
			SessionAgentCall{SessionID: sessionID},
			edgeInput{result: writeResult(), currentAssistant: &message.Message{Role: message.Assistant}})
		require.False(t, queued)
		require.Equal(t, 1, spy.calls)
	})

	t.Run("unparseable verdict degrades", func(t *testing.T) {
		t.Parallel()
		spy := &advisorSpyModel{text: "looks fine to me"}
		a, _, sessionID := reviewAgent(t, spy)
		queued := runEdgesForTest(a, t.Context(),
			SessionAgentCall{SessionID: sessionID},
			edgeInput{result: writeResult(), currentAssistant: &message.Message{Role: message.Assistant}})
		require.False(t, queued)
	})

	t.Run("nil model set degrades", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		a.freshReview = true // a.models stays nil — the degrade path.
		queued := runEdgesForTest(a, t.Context(),
			SessionAgentCall{SessionID: sessionID},
			edgeInput{result: writeResult(), currentAssistant: &message.Message{Role: message.Assistant}})
		require.False(t, queued)
	})

	t.Run("a write-less disproof exits degraded with the findings-stand mark", func(t *testing.T) {
		t.Parallel()
		spy := &advisorSpyModel{text: "VERDICT: ISSUES\nx"}
		a, svc, sessionID := reviewAgent(t, spy)
		assistant, err := svc.Create(t.Context(), sessionID, message.CreateMessageParams{
			Role:  message.Assistant,
			Parts: []message.ContentPart{message.TextContent{Text: "the diff is fine as-is"}},
		})
		require.NoError(t, err)

		// The turn answering the repair prompt wrote nothing — the
		// disproof claim alone must not silently close the loop.
		queued := runEdgesForTest(a, t.Context(),
			SessionAgentCall{
				SessionID: sessionID,
				Prompt:    freshReviewPromptSection(&edgeTrigger{report: "f.go — bug — reason"}),
			},
			edgeInput{result: cleanResult(), currentAssistant: &assistant})
		require.False(t, queued)
		require.Equal(t, 0, spy.calls)

		stored, err := svc.Get(t.Context(), assistant.ID)
		require.NoError(t, err)
		require.Contains(t, stored.JoinedText(), "the findings stand")
	})

	t.Run("arm off never calls the model", func(t *testing.T) {
		t.Parallel()
		spy := &advisorSpyModel{text: "VERDICT: ISSUES\nx"}
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		a.models = func(string) ModelSet { return ModelSet{Large: Model{Model: spy}} }
		queued := runEdgesForTest(a, t.Context(),
			SessionAgentCall{SessionID: sessionID},
			edgeInput{result: writeResult(), currentAssistant: &message.Message{Role: message.Assistant}})
		require.False(t, queued)
		require.Equal(t, 0, spy.calls)
	})

	t.Run("a write-less disproof exits marked, not silent", func(t *testing.T) {
		t.Parallel()
		// The repair prompt offers "fix or disprove" — a repair
		// turn that answers write-lessly exits the loop on its own
		// claim. The escape stands (re-reviewing an unchanged diff
		// could only repeat the verdict) but it must be marked:
		// degraded row, terminal line, zero review calls.
		spy := &advisorSpyModel{text: "VERDICT: ISSUES\nf.go — bug — reason"}
		a, _, sessionID := reviewAgent(t, spy)
		assistant := &message.Message{
			Role:  message.Assistant,
			Parts: []message.ContentPart{message.TextContent{Text: "the finding is wrong because"}},
		}
		queued := runEdgesForTest(a, t.Context(),
			SessionAgentCall{
				SessionID: sessionID,
				Prompt:    freshReviewPrefix + " — it saw the task:\n\nf.go — bug — reason",
			},
			edgeInput{result: cleanResult(), currentAssistant: assistant})
		require.False(t, queued)
		require.Equal(t, 0, spy.calls)
		require.Contains(t, assistant.Parts[0].(message.TextContent).Text, "findings stand")
	})

	t.Run("arm off leaves a write-less repair exit alone", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		tr := a.scanFreshReviewEdge(t.Context(),
			SessionAgentCall{SessionID: sessionID, Prompt: freshReviewPrefix + " x"},
			edgeInput{result: cleanResult()})
		require.Nil(t, tr)
	})
}

func TestParseFreshVerdict(t *testing.T) {
	t.Parallel()
	require.True(t, parseFreshVerdict("VERDICT: PASS").passed)
	require.True(t, parseFreshVerdict("VERDICT: PASS\nwhy not").passed)
	// Prefix noise must not read as a clear.
	require.True(t, parseFreshVerdict("VERDICT: PASSWORD").degraded)
	require.True(t, parseFreshVerdict("VERDICT: PASSING").degraded)
	v := parseFreshVerdict("VERDICT: ISSUES\nf.go — bug — reason")
	require.False(t, v.passed)
	require.False(t, v.degraded)
	require.Equal(t, "f.go — bug — reason", v.issues)
	require.True(t, parseFreshVerdict("VERDICT: ISSUES").degraded) // no findings
	require.True(t, parseFreshVerdict("VERDICT: MAYBE").degraded)
	require.True(t, parseFreshVerdict("VERDICT: PASSWORD").degraded) // prefix ≠ pass
	require.True(t, parseFreshVerdict("").degraded)
}

func TestFreshReviewDiff(t *testing.T) {
	t.Parallel()

	t.Run("outside a worktree reports unavailable", func(t *testing.T) {
		t.Parallel()
		out := freshReviewDiff(t.Context(), t.TempDir())
		require.Contains(t, out, "unavailable")
	})

	t.Run("empty dir reports unavailable", func(t *testing.T) {
		t.Parallel()
		require.Contains(t, freshReviewDiff(t.Context(), ""), "unavailable")
	})

	t.Run("a real repo renders diff plus untracked heads", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		git := func(args ...string) {
			cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, string(out))
		}
		git("init")
		git("config", "user.email", "t@t")
		git("config", "user.name", "t")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "tracked.go"), []byte("package x\n"), 0o644))
		git("add", ".")
		git("commit", "-m", "init")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "tracked.go"), []byte("package x\n// changed\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "new.go"), []byte("package x\n// brand new\n"), 0o644))

		out := freshReviewDiff(t.Context(), dir)
		require.Contains(t, out, "changed")
		require.Contains(t, out, "new file: new.go")
		require.Contains(t, out, "brand new")
	})

	t.Run("an untracked symlink never leaks its target", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		git := func(args ...string) {
			cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, string(out))
		}
		git("init")
		git("config", "user.email", "t@t")
		git("config", "user.name", "t")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "tracked.go"), []byte("package x\n"), 0o644))
		git("add", ".")
		git("commit", "-m", "init")

		outside := t.TempDir()
		secret := filepath.Join(outside, "secret.txt")
		require.NoError(t, os.WriteFile(secret, []byte("TOP-SECRET-VALUE"), 0o644))
		require.NoError(t, os.Symlink(secret, filepath.Join(dir, "leak.txt")))

		out := freshReviewDiff(t.Context(), dir)
		require.NotContains(t, out, "TOP-SECRET-VALUE")
		require.NotContains(t, out, "new file: leak.txt")
	})
}

func TestFreshReviewPromptSection(t *testing.T) {
	t.Parallel()
	s := freshReviewPromptSection(&edgeTrigger{report: "f.go — bug — reason"})
	require.True(t, strings.HasPrefix(s, freshReviewPrefix))
	require.Contains(t, s, "f.go — bug — reason")
	require.True(t, IsRepairPrompt(s))
	require.Empty(t, freshReviewPromptSection(&edgeTrigger{}))
	require.NotEmpty(t, freshReviewExhaustNote(&edgeTrigger{report: "x — y — z"}, 2))
}

// The edge must be registered last in the set — the final-state judge.
func TestFreshReviewRegisteredLast(t *testing.T) {
	t.Parallel()
	edges := (&sessionAgent{}).runEdgeSet()
	require.Equal(t, "fresh-review", edges[len(edges)-1].name)
}
