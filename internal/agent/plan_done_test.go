package agent

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// mkWrite lands a write-tool call/result pair in the session's stored
// history — the evidence record the done-scan reads.
func mkWrite(t *testing.T, svc message.Service, sessionID, callID, path, metadata string) {
	t.Helper()
	mkMsg(t, svc, sessionID, message.Assistant,
		message.ToolCall{ID: callID, Name: "edit", Input: fmt.Sprintf(`{"file_path":%q}`, path), Finished: true})
	mkMsg(t, svc, sessionID, message.Tool,
		message.ToolResult{ToolCallID: callID, Name: "edit", Content: "ok", Metadata: metadata})
}

// mkBashWrite lands a bash call/result pair — redirect targets count
// as observed writes even though no file tool ran.
func mkBashWrite(t *testing.T, svc message.Service, sessionID, callID, command string) {
	t.Helper()
	mkMsg(t, svc, sessionID, message.Assistant,
		message.ToolCall{ID: callID, Name: "bash", Input: fmt.Sprintf(`{"command":%q}`, command), Finished: true})
	mkMsg(t, svc, sessionID, message.Tool,
		message.ToolResult{ToolCallID: callID, Name: "bash", Content: "ok"})
}

func setPlan(t *testing.T, a *sessionAgent, sessionID string, items ...session.PlanItem) {
	t.Helper()
	sess, err := a.sessions.Get(t.Context(), sessionID)
	require.NoError(t, err)
	sess.Todos = items
	_, err = a.sessions.Save(t.Context(), sess)
	require.NoError(t, err)
}

func TestPlanVerdicts(t *testing.T) {
	t.Parallel()

	t.Run("open item is unresolved", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "do the work", Status: session.PlanItemPending},
		)
		verdicts := a.planVerdicts(t.Context(), sessionID)
		require.Len(t, verdicts, 1)
		require.Equal(t, planOpen, verdicts[0].state)
		require.True(t, verdicts[0].ready)
	})

	t.Run("completed mark without evidence is done", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "legacy item", Status: session.PlanItemCompleted},
		)
		require.Empty(t, a.planVerdicts(t.Context(), sessionID))
	})

	t.Run("bound check that never ran is evidence unmet", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "verified work", Status: session.PlanItemCompleted,
				EvidenceChecks: []string{"verify:build"}},
		)
		verdicts := a.planVerdicts(t.Context(), sessionID)
		require.Len(t, verdicts, 1)
		require.Equal(t, planEvidenceBlocked, verdicts[0].state)
		require.Contains(t, verdicts[0].reason, "evidence unmet")
	})

	t.Run("failed check blocks the completed mark", func(t *testing.T) {
		t.Parallel()
		a, svc, sessionID := newGateTestAgent(t, &config.Config{})
		mkWrite(t, svc, sessionID, "w1", "a.go",
			`{"verification":[{"check":"verify:build","state":"failed","detail":"exit code 1"}]}`)
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "verified work", Status: session.PlanItemCompleted,
				EvidenceChecks: []string{"verify:build"}},
		)
		verdicts := a.planVerdicts(t.Context(), sessionID)
		require.Len(t, verdicts, 1)
		require.Equal(t, planEvidenceBlocked, verdicts[0].state)
		require.Contains(t, verdicts[0].reason, "verify:build failed")
	})

	t.Run("green check completes the item", func(t *testing.T) {
		t.Parallel()
		a, svc, sessionID := newGateTestAgent(t, &config.Config{})
		mkWrite(t, svc, sessionID, "w1", "a.go",
			`{"verification":[{"check":"verify:build","state":"passed"}]}`)
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "verified work", Status: session.PlanItemCompleted,
				EvidenceChecks: []string{"verify:build"}},
		)
		require.Empty(t, a.planVerdicts(t.Context(), sessionID))
	})

	t.Run("regressed check reopens the item — latest instance wins", func(t *testing.T) {
		t.Parallel()
		a, svc, sessionID := newGateTestAgent(t, &config.Config{})
		mkWrite(t, svc, sessionID, "w1", "a.go",
			`{"verification":[{"check":"verify:build","state":"passed"}]}`)
		mkWrite(t, svc, sessionID, "w2", "b.go",
			`{"verification":[{"check":"verify:build","state":"failed","detail":"exit code 1"}]}`)
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "verified work", Status: session.PlanItemCompleted,
				EvidenceChecks: []string{"verify:build"}},
		)
		verdicts := a.planVerdicts(t.Context(), sessionID)
		require.Len(t, verdicts, 1)
		require.Equal(t, planEvidenceBlocked, verdicts[0].state)
	})

	t.Run("write on the declared path is done", func(t *testing.T) {
		t.Parallel()
		a, svc, sessionID := newGateTestAgent(t, &config.Config{})
		mkWrite(t, svc, sessionID, "w1", "a.go",
			`{"verification":[{"check":"diagnostics","state":"passed"}]}`)
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "edit a.go", Status: session.PlanItemCompleted,
				EvidencePaths: []string{"a.go"}},
		)
		require.Empty(t, a.planVerdicts(t.Context(), sessionID))
	})

	t.Run("no observed write on the declared path blocks", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "edit a.go", Status: session.PlanItemCompleted,
				EvidencePaths: []string{"a.go"}},
		)
		verdicts := a.planVerdicts(t.Context(), sessionID)
		require.Len(t, verdicts, 1)
		require.Equal(t, planEvidenceBlocked, verdicts[0].state)
		require.Contains(t, verdicts[0].reason, "no write observed on a.go")
	})

	t.Run("directory binding covers a nested write", func(t *testing.T) {
		t.Parallel()
		a, svc, sessionID := newGateTestAgent(t, &config.Config{})
		mkWrite(t, svc, sessionID, "w1", "pkg/f.go",
			`{"verification":[{"check":"diagnostics","state":"passed"}]}`)
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "work in pkg", Status: session.PlanItemCompleted,
				EvidencePaths: []string{"pkg"}},
		)
		require.Empty(t, a.planVerdicts(t.Context(), sessionID))
	})

	t.Run("failed package test covers every file in the dir", func(t *testing.T) {
		t.Parallel()
		a, svc, sessionID := newGateTestAgent(t, &config.Config{})
		mkWrite(t, svc, sessionID, "w1", "pkg/f.go",
			`{"verification":[{"check":"diagnostics","state":"passed"}]}`)
		mkWrite(t, svc, sessionID, "w2", "pkg/g.go",
			`{"verification":[{"check":"package-test:pkg","state":"failed","detail":"exit code 1"}]}`)
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "edit f.go", Status: session.PlanItemCompleted,
				EvidencePaths: []string{"pkg/f.go"}},
		)
		verdicts := a.planVerdicts(t.Context(), sessionID)
		require.Len(t, verdicts, 1)
		require.Equal(t, planEvidenceBlocked, verdicts[0].state)
		require.Contains(t, verdicts[0].reason, "package-test:pkg")
	})

	t.Run("dep on an unfinished item is not ready", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		depID := session.MintPlanItemID("setup", "")
		setPlan(t, a, sessionID,
			session.PlanItem{ID: depID, Key: "setup", Content: "setup work", Status: session.PlanItemPending},
			session.PlanItem{ID: "i2", Content: "downstream", Status: session.PlanItemPending, DependsOn: []string{depID}},
		)
		verdicts := a.planVerdicts(t.Context(), sessionID)
		require.Len(t, verdicts, 2)
		for _, v := range verdicts {
			if v.item.ID == "i2" {
				require.False(t, v.ready, "dep target is open — the item is not ready")
			} else {
				require.True(t, v.ready)
			}
		}
	})

	t.Run("superseded diagnostics failure on the path does not block", func(t *testing.T) {
		t.Parallel()
		a, svc, sessionID := newGateTestAgent(t, &config.Config{})
		// First write fails diagnostics; the fix lands with a second
		// write that resolves green — the stale entry must not block.
		mkWrite(t, svc, sessionID, "w1", "a.go",
			`{"verification":[{"check":"diagnostics","state":"failed","detail":"2 new error(s)"}]}`)
		mkWrite(t, svc, sessionID, "w2", "a.go",
			`{"verification":[{"check":"diagnostics","state":"passed"}]}`)
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "edit a.go", Status: session.PlanItemCompleted,
				EvidencePaths: []string{"a.go"}},
		)
		require.Empty(t, a.planVerdicts(t.Context(), sessionID))
	})

	t.Run("superseded named check on a covered write does not block", func(t *testing.T) {
		t.Parallel()
		a, svc, sessionID := newGateTestAgent(t, &config.Config{})
		mkWrite(t, svc, sessionID, "w1", "a.go",
			`{"verification":[{"check":"verify:build","state":"failed"}]}`)
		mkWrite(t, svc, sessionID, "w2", "b.go",
			`{"verification":[{"check":"verify:build","state":"passed"}]}`)
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "edit a.go", Status: session.PlanItemCompleted,
				EvidencePaths: []string{"a.go"}},
		)
		require.Empty(t, a.planVerdicts(t.Context(), sessionID))
	})

	t.Run("diagnostics pass on one file does not forgive another's failure", func(t *testing.T) {
		t.Parallel()
		a, svc, sessionID := newGateTestAgent(t, &config.Config{})
		mkWrite(t, svc, sessionID, "w1", "pkg/f.go",
			`{"verification":[{"check":"diagnostics","state":"failed","detail":"1 new error(s)"}]}`)
		mkWrite(t, svc, sessionID, "w2", "pkg/g.go",
			`{"verification":[{"check":"diagnostics","state":"passed"}]}`)
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "work in pkg", Status: session.PlanItemCompleted,
				EvidencePaths: []string{"pkg"}},
		)
		verdicts := a.planVerdicts(t.Context(), sessionID)
		require.Len(t, verdicts, 1)
		require.Equal(t, planEvidenceBlocked, verdicts[0].state)
		require.Contains(t, verdicts[0].reason, "pkg/f.go")
	})

	t.Run("pending covering check blocks path evidence", func(t *testing.T) {
		t.Parallel()
		a, svc, sessionID := newGateTestAgent(t, &config.Config{})
		mkWrite(t, svc, sessionID, "w1", "a.go",
			`{"verification":[{"check":"verify:build","state":"pending"}]}`)
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "edit a.go", Status: session.PlanItemCompleted,
				EvidencePaths: []string{"a.go"}},
		)
		verdicts := a.planVerdicts(t.Context(), sessionID)
		require.Len(t, verdicts, 1)
		require.Equal(t, planEvidenceBlocked, verdicts[0].state)
		require.Contains(t, verdicts[0].reason, "has not resolved")
	})

	t.Run("checkless rewrite supersedes a diagnostics failure", func(t *testing.T) {
		t.Parallel()
		a, svc, sessionID := newGateTestAgent(t, &config.Config{})
		mkWrite(t, svc, sessionID, "w1", "a.go",
			`{"verification":[{"check":"diagnostics","state":"failed","detail":"1 new error(s)"}]}`)
		// A bash rewrite carries no diagnostics entry — the stale
		// verdict is superseded, not latched.
		mkBashWrite(t, svc, sessionID, "b1", "cat > a.go <<'EOF'\nfixed\nEOF")
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "fix a.go", Status: session.PlanItemCompleted,
				EvidencePaths: []string{"a.go"}},
		)
		require.Empty(t, a.planVerdicts(t.Context(), sessionID))
	})

	t.Run("bash redirect write satisfies the declared path", func(t *testing.T) {
		t.Parallel()
		a, svc, sessionID := newGateTestAgent(t, &config.Config{})
		mkBashWrite(t, svc, sessionID, "b1", "cat > out.txt <<'EOF'\ncontent\nEOF")
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "create out.txt", Status: session.PlanItemCompleted,
				EvidencePaths: []string{"out.txt"}},
		)
		require.Empty(t, a.planVerdicts(t.Context(), sessionID))
	})

	t.Run("phantom redirect targets fabricate no write", func(t *testing.T) {
		t.Parallel()
		a, svc, sessionID := newGateTestAgent(t, &config.Config{})
		// Comparisons, expansions, and heredoc bodies look like `>`
		// writes to a naive scan — none is a real write, so the
		// binding must stay unmet.
		mkBashWrite(t, svc, sessionID, "b1", "[[ $x > out.txt ]]")
		mkBashWrite(t, svc, sessionID, "b2", "echo $((a > b)) > /dev/null")
		mkBashWrite(t, svc, sessionID, "b3", "cmd > $OUT; cmd2 > ~/o")
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "create out.txt", Status: session.PlanItemCompleted,
				EvidencePaths: []string{"out.txt"}},
		)
		verdicts := a.planVerdicts(t.Context(), sessionID)
		require.Len(t, verdicts, 1)
		require.Equal(t, planEvidenceBlocked, verdicts[0].state)
		require.Contains(t, verdicts[0].reason, "no write observed")
	})

	t.Run("phantom redirect cannot clear a diagnostics failure", func(t *testing.T) {
		t.Parallel()
		a, svc, sessionID := newGateTestAgent(t, &config.Config{})
		mkWrite(t, svc, sessionID, "w1", "a.go",
			`{"verification":[{"check":"diagnostics","state":"failed","detail":"1 new error(s)"}]}`)
		// A comparison that scans as `> a.go` must not supersede the
		// real diagnostics failure on a.go.
		mkBashWrite(t, svc, sessionID, "b1", "[[ $x > a.go ]]")
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "fix a.go", Status: session.PlanItemCompleted,
				EvidencePaths: []string{"a.go"}},
		)
		verdicts := a.planVerdicts(t.Context(), sessionID)
		require.Len(t, verdicts, 1)
		require.Equal(t, planEvidenceBlocked, verdicts[0].state)
		require.Contains(t, verdicts[0].reason, "diagnostics failed")
	})

	t.Run("errored bash result records no write", func(t *testing.T) {
		t.Parallel()
		a, svc, sessionID := newGateTestAgent(t, &config.Config{})
		mkMsg(t, svc, sessionID, message.Assistant,
			message.ToolCall{ID: "b1", Name: "bash", Input: `{"command":"cmd > out.txt"}`, Finished: true})
		mkMsg(t, svc, sessionID, message.Tool,
			message.ToolResult{ToolCallID: "b1", Name: "bash", Content: "exit 1", IsError: true})
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "create out.txt", Status: session.PlanItemCompleted,
				EvidencePaths: []string{"out.txt"}},
		)
		verdicts := a.planVerdicts(t.Context(), sessionID)
		require.Len(t, verdicts, 1)
		require.Equal(t, planEvidenceBlocked, verdicts[0].state)
		require.Contains(t, verdicts[0].reason, "no write observed")
	})

	t.Run("lsp rename records no path evidence", func(t *testing.T) {
		t.Parallel()
		a, svc, sessionID := newGateTestAgent(t, &config.Config{})
		// `path` on an lsp_rename call is the search root, not a
		// written file — it must not satisfy a binding to itself.
		mkMsg(t, svc, sessionID, message.Assistant,
			message.ToolCall{ID: "r1", Name: "lsp_rename", Input: `{"path":"pkg","old_name":"foo","new_name":"bar"}`, Finished: true})
		mkMsg(t, svc, sessionID, message.Tool,
			message.ToolResult{ToolCallID: "r1", Name: "lsp_rename", Content: "ok"})
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "rename in pkg", Status: session.PlanItemCompleted,
				EvidencePaths: []string{"pkg"}},
		)
		verdicts := a.planVerdicts(t.Context(), sessionID)
		require.Len(t, verdicts, 1)
		require.Equal(t, planEvidenceBlocked, verdicts[0].state)
		require.Contains(t, verdicts[0].reason, "no write observed")
	})

	t.Run("download target satisfies the declared path", func(t *testing.T) {
		t.Parallel()
		a, svc, sessionID := newGateTestAgent(t, &config.Config{})
		mkMsg(t, svc, sessionID, message.Assistant,
			message.ToolCall{ID: "d1", Name: "download", Input: `{"url":"https://x/y","file_path":"dl.go"}`, Finished: true})
		mkMsg(t, svc, sessionID, message.Tool,
			message.ToolResult{ToolCallID: "d1", Name: "download", Content: "ok"})
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "fetch dl.go", Status: session.PlanItemCompleted,
				EvidencePaths: []string{"dl.go"}},
		)
		require.Empty(t, a.planVerdicts(t.Context(), sessionID))
	})

	t.Run("directory binding with an extension-like name still reads as a dir", func(t *testing.T) {
		t.Parallel()
		a, svc, sessionID := newGateTestAgent(t, &config.Config{})
		mkWrite(t, svc, sessionID, "w1", "foo.d/x.go",
			`{"verification":[{"check":"diagnostics","state":"passed"}]}`)
		mkWrite(t, svc, sessionID, "w2", "foo.d/y.go",
			`{"verification":[{"check":"package-test:foo.d","state":"failed"}]}`)
		setPlan(t, a, sessionID,
			session.PlanItem{ID: "i1", Content: "work in foo.d", Status: session.PlanItemCompleted,
				EvidencePaths: []string{"foo.d"}},
		)
		verdicts := a.planVerdicts(t.Context(), sessionID)
		require.Len(t, verdicts, 1)
		require.Equal(t, planEvidenceBlocked, verdicts[0].state)
		require.Contains(t, verdicts[0].reason, "package-test:foo.d")
	})
}

func TestScanTodosEdgeEvidenceBlocked(t *testing.T) {
	t.Parallel()
	a, _, sessionID := newGateTestAgent(t, &config.Config{})
	setPlan(t, a, sessionID,
		session.PlanItem{ID: "i1", Content: "claimed done", Status: session.PlanItemCompleted,
			EvidenceChecks: []string{"verify:build"}},
	)
	result := &fantasy.AgentResult{Steps: []fantasy.StepResult{
		stepWith(fantasy.FinishReasonStop, fantasy.TextContent{Text: "done"}),
	}}
	queued := a.runEdges(t.Context(), SessionAgentCall{
		SessionID: sessionID, RunID: "run-1",
	}, edgeInput{result: result, currentAssistant: &message.Message{Role: message.Assistant}})
	require.True(t, queued, "evidence-blocked item must fire the run-end edge")
	q, _ := a.messageQueue.Get(sessionID)
	require.Len(t, q, 1)
	require.Contains(t, q[0].Prompt, "marked completed but")
	require.Contains(t, q[0].Prompt, "evidence unmet")
}

func TestTodosRetrySectionOrdersReadyBeforeBlocked(t *testing.T) {
	t.Parallel()
	trigger := &edgeTrigger{plan: []planVerdict{
		{item: session.PlanItem{ID: "i2", Content: "downstream", Status: session.PlanItemPending,
			DependsOn: []string{"depid"}}, state: planOpen, ready: false},
		{item: session.PlanItem{ID: "depid", Key: "setup", Content: "setup work",
			Status: session.PlanItemPending}, state: planOpen, ready: true},
		{item: session.PlanItem{ID: "i3", Content: "claimed done", Status: session.PlanItemCompleted},
			state: planEvidenceBlocked, reason: "verify:build failed"},
	}}
	out := todosRetrySection(trigger)
	require.True(t, strings.HasPrefix(out, todosRetryPrefix))

	ready := strings.Index(out, "setup work")
	blocked := strings.Index(out, "claimed done")
	waiting := strings.Index(out, "downstream")
	require.True(t, ready > 0 && blocked > ready && waiting > blocked,
		"expected ready → evidence-blocked → dep-blocked order, got:\n%s", out)
	require.Contains(t, out, "(blocked by: setup)")
	require.Contains(t, out, "marked completed but verify:build failed")
}
