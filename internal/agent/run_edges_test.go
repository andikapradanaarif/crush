package agent

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/notebook"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// stallResult builds a run that genuinely trips the loop detector —
// maxRepeats+1 identical tool interactions inside the window — so the
// signature-returning detector names the repeated tool. The terminal
// step is not a clean stop: the edge does not require one.
func stallResult() *fantasy.AgentResult {
	var steps []fantasy.StepResult
	for i := 0; i <= loopDetectionWindowSize; i++ {
		steps = append(steps, stepWith(fantasy.FinishReasonToolCalls,
			fantasy.ToolCallContent{
				ToolCallID: "tc-v", ToolName: "view",
				Input: `{"file_path":"main.go"}`,
			},
			fantasy.ToolResultContent{
				ToolCallID: "tc-v", ToolName: "view",
				Result: fantasy.ToolResultOutputContentText{Text: "same output"},
			},
		))
	}
	steps = append(steps, stepWith(fantasy.FinishReasonUnknown, fantasy.TextContent{Text: "..."}))
	return &fantasy.AgentResult{Steps: steps}
}

// cleanResult builds a run that neither stalled nor repeated — the
// plain "no edge applies" fixture.
func cleanResult() *fantasy.AgentResult {
	return &fantasy.AgentResult{Steps: []fantasy.StepResult{
		stepWith(fantasy.FinishReasonToolCalls,
			fantasy.ToolCallContent{
				ToolCallID: "tc-g", ToolName: "grep",
				Input: `{"pattern":"x"}`,
			},
			fantasy.ToolResultContent{
				ToolCallID: "tc-g", ToolName: "grep",
				Result: fantasy.ToolResultOutputContentText{Text: "one match"},
			},
		),
		stepWith(fantasy.FinishReasonStop, fantasy.TextContent{Text: "done"}),
	}}
}

func TestStallEdge(t *testing.T) {
	t.Parallel()

	t.Run("first stall queues a replan retry", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		a.ambiguityClarification = true
		a.interactive = true
		a.tools = csync.NewSliceFrom([]fantasy.AgentTool{
			&fakeTool{name: tools.TodosToolName},
			&fakeTool{name: tools.QuestionToolName},
		})
		queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID, RunID: "r", RunStamp: 42},
			edgeInput{result: stallResult(), stalled: true})
		require.True(t, queued)
		q, _ := a.messageQueue.Get(sessionID)
		require.Len(t, q, 1)
		require.Equal(t, "r", q[0].RunID)
		require.Equal(t, 1, q[0].RepairAttempts)
		// The retry clone keeps the turn's stamp so the scope gate
		// does not re-arm mid-turn.
		require.Equal(t, uint64(42), q[0].RunStamp)
		require.True(t, strings.HasPrefix(q[0].Prompt, stallReplanPrefix))
		require.Contains(t, q[0].Prompt, "What's blocking:")
		require.NotContains(t, q[0].Prompt, "question-tool")
	})

	t.Run("stall on a repair turn queues the escalation", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		a.ambiguityClarification = true
		a.interactive = true
		a.tools = csync.NewSliceFrom([]fantasy.AgentTool{
			&fakeTool{name: tools.TodosToolName},
			&fakeTool{name: tools.QuestionToolName},
		})
		queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID, RepairAttempts: 1},
			edgeInput{result: stallResult(), stalled: true})
		require.True(t, queued)
		q, _ := a.messageQueue.Get(sessionID)
		require.Len(t, q, 1)
		require.Equal(t, 2, q[0].RepairAttempts)
		require.True(t, strings.HasPrefix(q[0].Prompt, stallRetryPrefix))
		require.Contains(t, q[0].Prompt, "question")
	})

	t.Run("headless first stall enqueues the replan", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		a.ambiguityClarification = true
		queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID, NonInteractive: true},
			edgeInput{result: stallResult(), stalled: true})
		require.True(t, queued, "the replan branch is a model retry — it fires headless")
		q, _ := a.messageQueue.Get(sessionID)
		require.Len(t, q, 1)
		require.True(t, strings.HasPrefix(q[0].Prompt, stallReplanPrefix))
	})

	t.Run("headless escalate lands a blocker report, not a retry", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		a.ambiguityClarification = true
		asst := &message.Message{Role: message.Assistant}
		queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID, NonInteractive: true, RepairAttempts: 1},
			edgeInput{result: stallResult(), currentAssistant: asst, stalled: true})
		require.False(t, queued)
		q, _ := a.messageQueue.Get(sessionID)
		require.Empty(t, q)
		require.Contains(t, asst.Content().Text, "Stopped:")
		require.Contains(t, asst.Content().Text, "What's blocking:")
		require.Contains(t, asst.Content().Text, `"view"`)
	})

	t.Run("flag off leaves a silent stop", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: stallResult(), stalled: true})
		require.False(t, queued)
	})

	t.Run("non-stalled run does not fire the edge", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		a.ambiguityClarification = true
		queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: cleanResult()})
		require.False(t, queued)
	})

	t.Run("masked stall fires via the fallback re-scan", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		a.ambiguityClarification = true
		// in.stalled is false — the context-pressure summarize check
		// short-circuited the detector — but the repeated signature in
		// the steps still produces a replan.
		queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: stallResult()})
		require.True(t, queued)
		q, _ := a.messageQueue.Get(sessionID)
		require.Len(t, q, 1)
		require.True(t, strings.HasPrefix(q[0].Prompt, stallReplanPrefix))
	})

	t.Run("budget exhausted surfaces the terminal note", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		a.ambiguityClarification = true
		a.interactive = true
		a.tools = csync.NewSliceFrom([]fantasy.AgentTool{
			&fakeTool{name: tools.TodosToolName},
			&fakeTool{name: tools.QuestionToolName},
		})
		asst := &message.Message{Role: message.Assistant}
		queued := runEdgesForTest(a, t.Context(), SessionAgentCall{
			SessionID: sessionID, RepairAttempts: maxRepairAttempts,
		}, edgeInput{result: stallResult(), currentAssistant: asst, stalled: true})
		require.False(t, queued)
		require.Contains(t, asst.Content().Text, "repair attempt")
		// The structured blocker report survives exhaustion too —
		// interactive runs get the same what's-blocking detail
		// headless runs get from resolve.
		require.Contains(t, asst.Content().Text, "What's blocking:")
	})

	t.Run("one escalation per blocker via the shared budget", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		a.ambiguityClarification = true
		a.interactive = true
		a.tools = csync.NewSliceFrom([]fantasy.AgentTool{
			&fakeTool{name: tools.TodosToolName},
			&fakeTool{name: tools.QuestionToolName},
		})
		asst := &message.Message{Role: message.Assistant}
		for attempt := 0; attempt < maxRepairAttempts; attempt++ {
			queued := runEdgesForTest(a, t.Context(), SessionAgentCall{
				SessionID: sessionID, RepairAttempts: attempt,
			}, edgeInput{result: stallResult(), currentAssistant: asst, stalled: true})
			require.True(t, queued, "attempt %d should still have budget", attempt)
		}
		q, _ := a.messageQueue.Get(sessionID)
		require.Len(t, q, maxRepairAttempts)
		// Retries prepend — the newest (most-consumed budget) is first.
		require.Equal(t, maxRepairAttempts, q[0].RepairAttempts)
	})
}

// TestRepairPromptPrefixes_Stable pins the repair-prompt wording to
// the exported RepairPromptPrefixes — the eval analyzer fingerprints
// those literals to keep repair turns inside the firing process-turn.
func TestRepairPromptPrefixes_Stable(t *testing.T) {
	t.Parallel()
	v := (&sessionAgent{}).verificationRetrySection(&edgeTrigger{failed: []gateCheckOutcome{{
		check: message.VerificationCheck{Check: "c"}, output: "out",
	}}})
	require.True(t, strings.HasPrefix(v, verificationRetryPrefix))
	require.True(t, strings.HasPrefix(
		todosRetrySection(&edgeTrigger{plan: []planVerdict{{
			item: session.PlanItem{Content: "x", Status: session.PlanItemPending},
		}}}),
		todosRetryPrefix))
	require.True(t, strings.HasPrefix(stallRetrySection(nil), stallRetryPrefix))
	// The replan branch fingerprints separately — the eval analyzer
	// splits stall variants on the leading literal.
	replan := stallReplanSection(&edgeTrigger{report: "Stopped: x"})
	require.True(t, strings.HasPrefix(replan, stallReplanPrefix))
	require.NotEqual(t, stallReplanPrefix, stallRetryPrefix)
	require.True(t, strings.HasPrefix(
		burnWatchRetrySection(&edgeTrigger{inputTokens: 250000, steps: make([]fantasy.StepResult, 40)}),
		burnWatchPrefix))
	// No prefix is a prefix of another — a merged prompt reads as its
	// leading edge only.
	for i, p := range RepairPromptPrefixes {
		for j, q := range RepairPromptPrefixes {
			if i != j {
				require.False(t, strings.HasPrefix(q, p), "%q starts with %q", q, p)
			}
		}
	}
}

// TestFailedCheckGroups pins the per-file grouping: diagnostics
// entries minted by one write render as one failure with its shared
// output once — not one <check> block per affected file.
func TestFailedCheckGroups(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	a := &sessionAgent{configStore: config.NewTestStoreWithDir(&config.Config{}, dir)}
	mk := func(callID, check, path, detail, output string) gateCheckOutcome {
		return gateCheckOutcome{
			toolCallID: callID,
			check: message.VerificationCheck{
				Check: check, State: message.VerificationFailed,
				Path: filepath.Join(dir, path), Detail: detail,
			},
			output: output,
		}
	}
	failed := []gateCheckOutcome{
		mk("tc1", "diagnostics", "a.go", "1 new error(s)", "out-1"),
		mk("tc1", "diagnostics", "b.go", "2 new error(s)", "out-1"),
		mk("tc1", "diagnostics", "c.go", "1 new error(s)", "out-1"),
		mk("tc2", "diagnostics", "d.go", "1 new error(s)", "out-2"),
		mk("tc3", "verify:build", "", "", "out-3"),
	}

	groups := failedCheckGroups(failed)
	require.Len(t, groups, 3)
	require.Len(t, groups[0], 3)

	section := a.verificationRetrySection(&edgeTrigger{failed: failed})
	require.Equal(t, 2, strings.Count(section, `<check name="diagnostics">`))
	require.Equal(t, 1, strings.Count(section, "out-1"),
		"shared tool output must render once per group")
	// Per-file paths render relative to the working dir, matching the
	// plan reasons in the same prompt.
	require.Contains(t, section, "a.go: 1 new error(s)")
	require.NotContains(t, section, dir)

	note := verificationExhaustNote(&edgeTrigger{failed: failed}, 2)
	require.Contains(t, note, "3 check(s) still failing")
	require.Contains(t, note, "Last failure: out-3",
		"failures append chronologically — the note names the last, not the first")
}

// --- burn-watch edge + firing records + deferred carrier ---

// newEdgeTestAgent builds a sessionAgent wired to a real edge-firing
// store so the recorded rows and in-memory counters are assertable.
func newEdgeTestAgent(t *testing.T, cfg *config.Config) (*sessionAgent, *sql.DB, string) {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	sess, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)

	svc := message.NewService(q)
	a := &sessionAgent{
		configStore:  config.NewTestStore(cfg),
		sessions:     sessions,
		messages:     svc,
		tools:        csync.NewSliceFrom([]fantasy.AgentTool{&fakeTool{name: tools.TodosToolName}}),
		messageQueue: csync.NewMap[string, []SessionAgentCall](),
		dispatchMu:   csync.NewMap[string, *sync.Mutex](),
		edgeStore:    q,
		edgeStats:    csync.NewMap[string, map[string]int](),
	}
	return a, conn, sess.ID
}

type firingRow struct {
	edge    string
	variant string
	outcome string
	detail  string
	turnSeq int64
}

// firingRows reads back the recorded edge_firings for a session.
func firingRows(t *testing.T, conn *sql.DB, sessionID string) []firingRow {
	t.Helper()
	rows, err := conn.QueryContext(t.Context(),
		`SELECT edge, variant, outcome, trigger_detail, turn_seq FROM edge_firings WHERE session_id = ? ORDER BY edge, outcome`, sessionID)
	require.NoError(t, err)
	defer rows.Close()
	var out []firingRow
	for rows.Next() {
		var r firingRow
		require.NoError(t, rows.Scan(&r.edge, &r.variant, &r.outcome, &r.detail, &r.turnSeq))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func firingOutcome(t *testing.T, conn *sql.DB, sessionID, edge string) string {
	t.Helper()
	var latest firingRow
	for _, r := range firingRows(t, conn, sessionID) {
		if r.edge == edge && r.turnSeq >= latest.turnSeq {
			latest = r
		}
	}
	return latest.outcome
}

// burnResult builds a clean-stopping run of n exploration steps with
// the given input-token spend and no mutating calls. Steps vary their
// input so the loop detector's signature check stays quiet.
func burnResult(steps int, inputTokens int64) *fantasy.AgentResult {
	var s []fantasy.StepResult
	for i := 0; i < steps-1; i++ {
		id := fmt.Sprintf("tc-%d", i)
		s = append(s, stepWith(fantasy.FinishReasonToolCalls,
			fantasy.ToolCallContent{
				ToolCallID: id, ToolName: "view",
				Input: fmt.Sprintf(`{"file_path":"f%d.go"}`, i),
			},
			fantasy.ToolResultContent{
				ToolCallID: id, ToolName: "view",
				Result: fantasy.ToolResultOutputContentText{Text: fmt.Sprintf("content %d", i)},
			},
		))
	}
	s = append(s, stepWith(fantasy.FinishReasonStop, fantasy.TextContent{Text: "done"}))
	return &fantasy.AgentResult{Steps: s, TotalUsage: fantasy.Usage{InputTokens: inputTokens}}
}

func interactiveEdgeAgent(t *testing.T) (*sessionAgent, *sql.DB, string) {
	a, conn, sessionID := newEdgeTestAgent(t, &config.Config{})
	a.ambiguityClarification = true
	a.interactive = true
	a.tools = csync.NewSliceFrom([]fantasy.AgentTool{
		&fakeTool{name: tools.TodosToolName},
		&fakeTool{name: tools.QuestionToolName},
	})
	return a, conn, sessionID
}

func TestBurnWatchEdge(t *testing.T) {
	t.Parallel()

	t.Run("step-arm crossing escalates and stamps the marker", func(t *testing.T) {
		t.Parallel()
		a, conn, sessionID := interactiveEdgeAgent(t)
		queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID, RunID: "r", RunStamp: 7},
			edgeInput{result: burnResult(burnWatchStepsThreshold+1, 1000), turnSeq: 3})
		require.True(t, queued)
		q, _ := a.messageQueue.Get(sessionID)
		require.Len(t, q, 1)
		require.True(t, strings.HasPrefix(q[0].Prompt, burnWatchPrefix))
		require.Contains(t, q[0].Prompt, "question")
		require.True(t, q[0].burnWatched, "the fired outcome stamps the once-per-crossing marker")
		require.Equal(t, "fired", firingOutcome(t, conn, sessionID, "burn-watch"))
	})

	t.Run("token arm needs the step floor", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := interactiveEdgeAgent(t)
		// Steps at the floor + tokens over the arm fires.
		queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: burnResult(burnWatchMinSteps, burnWatchInputTokens+1)})
		require.True(t, queued)
		// Same tokens under the floor does not.
		queued = runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID + "x"},
			edgeInput{result: burnResult(burnWatchMinSteps-1, burnWatchInputTokens+1)})
		require.False(t, queued)
		// Steps under the arm with low spend does not.
		queued = runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: burnResult(burnWatchStepsThreshold-5, 500)})
		require.False(t, queued)
	})

	t.Run("mutating calls suppress the tripwire", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := interactiveEdgeAgent(t)
		result := burnResult(burnWatchStepsThreshold+1, 0)
		// A bash redirect write is mutating even though no write tool ran.
		result.Steps[0] = stepWith(fantasy.FinishReasonToolCalls,
			fantasy.ToolCallContent{
				ToolCallID: "tc-b", ToolName: "bash",
				Input: `{"command":"echo x > out.txt"}`,
			},
			fantasy.ToolResultContent{
				ToolCallID: "tc-b", ToolName: "bash",
				Result: fantasy.ToolResultOutputContentText{Text: ""},
			},
		)
		require.False(t, runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: result}))
	})

	t.Run("stalled runs stay with the stall edge", func(t *testing.T) {
		t.Parallel()
		a, conn, sessionID := interactiveEdgeAgent(t)
		queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: stallResult(), stalled: true})
		require.True(t, queued, "the stall edge still owns the boundary")
		q, _ := a.messageQueue.Get(sessionID)
		require.True(t, strings.HasPrefix(q[0].Prompt, stallReplanPrefix))
		require.Empty(t, firingOutcome(t, conn, sessionID, "burn-watch"),
			"burn-watch never scans a stalled boundary")
	})

	t.Run("headless degrade appends the assumption", func(t *testing.T) {
		t.Parallel()
		a, conn, sessionID := newEdgeTestAgent(t, &config.Config{})
		a.ambiguityClarification = true
		asst := &message.Message{Role: message.Assistant}
		queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID, NonInteractive: true},
			edgeInput{result: burnResult(burnWatchStepsThreshold+1, burnWatchInputTokens), currentAssistant: asst})
		require.False(t, queued)
		require.Contains(t, asst.Content().Text, "Burn-watch:")
		require.Contains(t, asst.Content().Text, "no file writes")
		require.Equal(t, "headless-degraded", firingOutcome(t, conn, sessionID, "burn-watch"))
	})

	t.Run("flag off records gated without acting", func(t *testing.T) {
		t.Parallel()
		a, conn, sessionID := interactiveEdgeAgent(t)
		a.ambiguityClarification = false
		asst := &message.Message{Role: message.Assistant}
		queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: burnResult(burnWatchStepsThreshold+1, 0), currentAssistant: asst})
		require.False(t, queued)
		require.Empty(t, asst.Content().Text, "gated must skip resolve — no assumption appended")
		require.Equal(t, "gated", firingOutcome(t, conn, sessionID, "burn-watch"))
	})

	t.Run("marker suppresses the second crossing", func(t *testing.T) {
		t.Parallel()
		a, conn, sessionID := interactiveEdgeAgent(t)
		queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID, burnWatched: true},
			edgeInput{result: burnResult(burnWatchStepsThreshold+1, 0)})
		require.False(t, queued)
		require.Equal(t, "suppressed", firingOutcome(t, conn, sessionID, "burn-watch"))
	})

	t.Run("a mutating run clears the marker off the clone", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := interactiveEdgeAgent(t)
		result := burnResult(5, 0)
		result.Steps[0] = stepWith(fantasy.FinishReasonToolCalls,
			fantasy.ToolCallContent{
				ToolCallID: "tc-e", ToolName: "edit",
				Input: `{"file_path":"f.go"}`,
			},
			fantasy.ToolResultContent{
				ToolCallID: "tc-e", ToolName: "edit",
				Result: fantasy.ToolResultOutputContentText{Text: "ok"},
			},
		)
		// The stamped call's run wrote — the marker clears before
		// scans, so a firing todos edge's clone is unstamped.
		sess, err := a.sessions.Get(t.Context(), sessionID)
		require.NoError(t, err)
		sess.Todos = []session.PlanItem{{Content: "open", Status: session.PlanItemPending}}
		_, err = a.sessions.Save(t.Context(), sess)
		require.NoError(t, err)
		queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID, burnWatched: true},
			edgeInput{result: result})
		require.True(t, queued)
		q, _ := a.messageQueue.Get(sessionID)
		require.False(t, q[0].burnWatched, "a write resets the write-less streak")
	})
}

func TestEdgeFiringRecords(t *testing.T) {
	t.Parallel()

	setTodos := func(t *testing.T, a *sessionAgent, sessionID string, items ...session.PlanItem) {
		t.Helper()
		sess, err := a.sessions.Get(t.Context(), sessionID)
		require.NoError(t, err)
		sess.Todos = items
		_, err = a.sessions.Save(t.Context(), sess)
		require.NoError(t, err)
	}

	t.Run("fired rows carry the full column set", func(t *testing.T) {
		t.Parallel()
		a, conn, sessionID := newEdgeTestAgent(t, &config.Config{})
		a.ambiguityClarification = true
		setTodos(t, a, sessionID, session.PlanItem{Content: "open", Status: session.PlanItemPending})
		queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID, RepairAttempts: 0, RunStamp: 99},
			edgeInput{result: cleanResult(), turnSeq: 4})
		require.True(t, queued)
		var variant, detail, outcome string
		var turnSeq, attempts, stamp int64
		require.NoError(t, conn.QueryRowContext(t.Context(),
			`SELECT variant, outcome, trigger_detail, turn_seq, repair_attempts, run_stamp
			 FROM edge_firings WHERE session_id = ? AND edge = 'todos'`, sessionID).
			Scan(&variant, &outcome, &detail, &turnSeq, &attempts, &stamp))
		require.Equal(t, "fired", outcome)
		require.Equal(t, int64(4), turnSeq)
		require.Equal(t, int64(0), attempts)
		require.Equal(t, int64(99), stamp)
		require.Equal(t, "open=1", detail)
	})

	t.Run("gated rows record the verdict flag-off", func(t *testing.T) {
		t.Parallel()
		a, conn, sessionID := newEdgeTestAgent(t, &config.Config{})
		asst := &message.Message{Role: message.Assistant}
		queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: stallResult(), currentAssistant: asst, stalled: true})
		require.False(t, queued)
		require.Empty(t, asst.Content().Text,
			"a gated stall must not append the blocker report")
		rows := firingRows(t, conn, sessionID)
		require.Len(t, rows, 1)
		require.Equal(t, "stall", rows[0].edge)
		require.Equal(t, "gated", rows[0].outcome)
		require.Contains(t, rows[0].detail, `tool="view"`,
			"the row carries the would-have-fired verdict's evidence")
	})

	t.Run("pending verification resolving clean records cleared", func(t *testing.T) {
		t.Parallel()
		a, conn, sessionID := newEdgeTestAgent(t, &config.Config{})
		result := &fantasy.AgentResult{Steps: []fantasy.StepResult{
			stepWith(fantasy.FinishReasonToolCalls, fantasy.ToolResultContent{
				ToolCallID: "tc-edit", ToolName: "edit",
				Result:         fantasy.ToolResultOutputContentText{Text: "edited"},
				ClientMetadata: `{"verification":[{"check":"verify:x","state":"pending"}]}`,
			}),
			stepWith(fantasy.FinishReasonStop, fantasy.TextContent{Text: "done"}),
		}}
		require.False(t, runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: result, currentAssistant: &message.Message{Role: message.Assistant}}))
		require.Equal(t, "cleared", firingOutcome(t, conn, sessionID, "verification"))
	})

	t.Run("mid-loop cancel writes cancelled rows for un-scanned edges", func(t *testing.T) {
		t.Parallel()
		a, conn, sessionID := newEdgeTestAgent(t, &config.Config{})
		result := &fantasy.AgentResult{Steps: []fantasy.StepResult{
			stepWith(fantasy.FinishReasonToolCalls, fantasy.ToolResultContent{
				ToolCallID: "tc-edit", ToolName: "edit",
				Result:         fantasy.ToolResultOutputContentText{Text: "edited"},
				ClientMetadata: `{"verification":[{"check":"verify:x","state":"pending"}]}`,
			}),
			stepWith(fantasy.FinishReasonStop, fantasy.TextContent{Text: "done"}),
		}}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		require.False(t, runEdgesForTest(a, ctx, SessionAgentCall{SessionID: sessionID},
			edgeInput{result: result, currentAssistant: &message.Message{Role: message.Assistant}}))
		outcomes := map[string]string{}
		for _, r := range firingRows(t, conn, sessionID) {
			outcomes[r.edge] = r.outcome
		}
		// The cancelled boundary leaves no clean reads: every edge in
		// the set gets a cancelled row.
		for _, edge := range []string{"verification", "todos", "stall", "burn-watch"} {
			require.Equal(t, "cancelled", outcomes[edge], "edge %s", edge)
		}
	})

	t.Run("turn_seq is the absolute user-message ordinal", func(t *testing.T) {
		t.Parallel()
		a, conn, sessionID := newEdgeTestAgent(t, &config.Config{})
		svc := a.messages
		for i := 0; i < 3; i++ {
			_, err := svc.Create(t.Context(), sessionID, message.CreateMessageParams{
				Role:  message.User,
				Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("prompt %d", i)}},
			})
			require.NoError(t, err)
		}
		require.Equal(t, int64(3), a.edgeTurnSeq(t.Context(), sessionID))
		setTodos(t, a, sessionID, session.PlanItem{Content: "open", Status: session.PlanItemPending})
		require.True(t, runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: cleanResult(), turnSeq: 3}))
		rows := firingRows(t, conn, sessionID)
		require.Len(t, rows, 1)
		require.Equal(t, int64(3), rows[0].turnSeq)
	})

	t.Run("same boundary dedupes via INSERT OR IGNORE", func(t *testing.T) {
		t.Parallel()
		a, conn, sessionID := newEdgeTestAgent(t, &config.Config{})
		setTodos(t, a, sessionID, session.PlanItem{Content: "open", Status: session.PlanItemPending})
		for i := 0; i < 2; i++ {
			runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID},
				edgeInput{result: cleanResult(), turnSeq: 7})
		}
		require.Len(t, firingRows(t, conn, sessionID), 1)
		// The deduped second write does not double-count telemetry.
		m, _ := a.edgeStats.Get(sessionID)
		require.Equal(t, 1, m["todos:fired"])
	})

	t.Run("exhausted rows record at the spent boundary", func(t *testing.T) {
		t.Parallel()
		a, conn, sessionID := newEdgeTestAgent(t, &config.Config{})
		setTodos(t, a, sessionID, session.PlanItem{Content: "open", Status: session.PlanItemPending})
		require.False(t, runEdgesForTest(a, t.Context(),
			SessionAgentCall{SessionID: sessionID, RepairAttempts: maxRepairAttempts},
			edgeInput{result: cleanResult(), currentAssistant: &message.Message{Role: message.Assistant}}))
		require.Equal(t, "exhausted", firingOutcome(t, conn, sessionID, "todos"))
	})
}

func TestDeferredCarrier(t *testing.T) {
	t.Parallel()

	setTodos := func(t *testing.T, a *sessionAgent, sessionID string, items ...session.PlanItem) {
		t.Helper()
		sess, err := a.sessions.Get(t.Context(), sessionID)
		require.NoError(t, err)
		sess.Todos = items
		_, err = a.sessions.Save(t.Context(), sess)
		require.NoError(t, err)
	}

	t.Run("escalate wins the slot; retry triggers ride the clone", func(t *testing.T) {
		t.Parallel()
		a, conn, sessionID := interactiveEdgeAgent(t)
		setTodos(t, a, sessionID, session.PlanItem{Content: "open", Status: session.PlanItemPending})
		queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: burnResult(burnWatchStepsThreshold+1, 0), turnSeq: 1})
		require.True(t, queued)
		q, _ := a.messageQueue.Get(sessionID)
		require.Len(t, q, 1)
		require.True(t, strings.HasPrefix(q[0].Prompt, burnWatchPrefix),
			"the escalation-family prompt wins the slot outright")
		require.Len(t, q[0].deferred, 1)
		require.Equal(t, "todos", q[0].deferred[0].edge.name)
		outcomes := map[string]string{}
		for _, r := range firingRows(t, conn, sessionID) {
			outcomes[r.edge] = r.outcome
		}
		require.Equal(t, "fired", outcomes["burn-watch"])
		require.Equal(t, "deferred", outcomes["todos"])
	})

	t.Run("carried trigger re-fires at the escalate run's clean boundary", func(t *testing.T) {
		t.Parallel()
		a, conn, sessionID := interactiveEdgeAgent(t)
		setTodos(t, a, sessionID, session.PlanItem{Content: "open", Status: session.PlanItemPending})
		require.True(t, runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: burnResult(burnWatchStepsThreshold+1, 0), turnSeq: 1}))
		q, _ := a.messageQueue.Get(sessionID)
		escalate := q[0]
		require.Len(t, escalate.deferred, 1)
		// The question was answered; the escalate run ends clean with
		// the todo still open — the deferred trigger re-fires on the
		// same clone's already-incremented budget.
		queued := runEdgesForTest(a, t.Context(), escalate,
			edgeInput{result: cleanResult(), turnSeq: 2})
		require.True(t, queued)
		q2, _ := a.messageQueue.Get(sessionID)
		require.Len(t, q2, 2, "the re-fired retry prepends ahead of the drained queue entry")
		require.Equal(t, 2, q2[0].RepairAttempts)
		require.True(t, strings.HasPrefix(q2[0].Prompt, todosRetryPrefix))
		require.Equal(t, "fired", firingOutcome(t, conn, sessionID, "todos"))
	})

	t.Run("session-state evidence cleared during escalation records cleared", func(t *testing.T) {
		t.Parallel()
		a, conn, sessionID := interactiveEdgeAgent(t)
		setTodos(t, a, sessionID, session.PlanItem{Content: "open", Status: session.PlanItemPending})
		require.True(t, runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: burnResult(burnWatchStepsThreshold+1, 0), turnSeq: 1}))
		q, _ := a.messageQueue.Get(sessionID)
		escalate := q[0]
		// The escalation turn resolved the todo — the carried
		// trigger's session-state evidence re-scans clean.
		setTodos(t, a, sessionID)
		require.False(t, runEdgesForTest(a, t.Context(), escalate,
			edgeInput{result: cleanResult(), turnSeq: 2}))
		require.Equal(t, "cleared", firingOutcome(t, conn, sessionID, "todos"))
	})

	t.Run("StopTurn-ended boundary renders the deferred note", func(t *testing.T) {
		t.Parallel()
		a, conn, sessionID := interactiveEdgeAgent(t)
		setTodos(t, a, sessionID, session.PlanItem{Content: "open", Status: session.PlanItemPending})
		require.True(t, runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: burnResult(burnWatchStepsThreshold+1, 0), turnSeq: 1}))
		q, _ := a.messageQueue.Get(sessionID)
		escalate := q[0]
		asst := &message.Message{Role: message.Assistant}
		stopTurn := &fantasy.AgentResult{Steps: []fantasy.StepResult{
			stepWith(fantasy.FinishReasonStop, fantasy.ToolResultContent{
				ToolCallID: "tc-q", ToolName: "question", StopTurn: true,
				Result: fantasy.ToolResultOutputContentText{Text: "User cancelled this question"},
			}),
		}}
		require.False(t, runEdgesForTest(a, t.Context(), escalate,
			edgeInput{result: stopTurn, currentAssistant: asst, turnSeq: 2}))
		require.Equal(t, "cancelled", firingOutcome(t, conn, sessionID, "todos"))
		require.Contains(t, asst.Content().Text, "todo item(s) still unresolved",
			"the deferred trigger's terminal note survives the cancelled boundary")
	})
}

func TestDeferredCarrierRecarry(t *testing.T) {
	t.Parallel()

	t.Run("carried trigger rides the stall retry past a non-clean boundary", func(t *testing.T) {
		t.Parallel()
		a, conn, sessionID := interactiveEdgeAgent(t)
		sess, err := a.sessions.Get(t.Context(), sessionID)
		require.NoError(t, err)
		sess.Todos = []session.PlanItem{{Content: "open", Status: session.PlanItemPending}}
		_, err = a.sessions.Save(t.Context(), sess)
		require.NoError(t, err)

		// A todos trigger deferred at an earlier boundary.
		carried := []deferredTrigger{{
			edge: a.runEdgeSet()[1],
			trigger: &edgeTrigger{
				plan:         []planVerdict{{item: sess.Todos[0], state: planOpen, ready: true}},
				fire:         true,
				sessionState: true,
				detail:       "open=1",
			},
		}}
		// The escalate run stalled — the stall edge fires its
		// escalation branch while the carried todos trigger finds no
		// clean slot.
		call := SessionAgentCall{SessionID: sessionID, RepairAttempts: 1, deferred: carried}
		queued := runEdgesForTest(a, t.Context(), call,
			edgeInput{result: stallResult(), stalled: true, turnSeq: 2})
		require.True(t, queued)
		q, _ := a.messageQueue.Get(sessionID)
		require.Len(t, q, 1)
		require.True(t, strings.HasPrefix(q[0].Prompt, stallRetryPrefix))
		require.Len(t, q[0].deferred, 1)
		require.Equal(t, "todos", q[0].deferred[0].edge.name,
			"the deferred trigger survives the stalled boundary on the clone")
		// The re-carried trigger records deferred at this boundary —
		// its lifecycle row is "still waiting", not clean.
		require.Equal(t, "deferred", firingOutcome(t, conn, sessionID, "todos"))
	})
}

func TestSessionTelemetryEdgeFirings(t *testing.T) {
	t.Parallel()

	coord := newSummaryTestCoordinator(t, `{
  "options": {"disable_default_providers": true, "disable_provider_auto_update": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`)
	sa, ok := coord.currentAgent().(*sessionAgent)
	require.True(t, ok)
	require.NotNil(t, sa.edgeStats)
	sa.edgeStats.Set("sess-1", map[string]int{
		"todos:fired":            2,
		"todos:exhausted":        1,
		"stall:gated":            3,
		"burn-watch:suppressed":  1,
		"verification:cancelled": 1,
	})

	tel := coord.SessionTelemetry("sess-1")
	require.Equal(t, 2, tel.EdgeFirings["todos"]["fired"])
	require.Equal(t, 1, tel.EdgeFirings["todos"]["exhausted"])
	require.Equal(t, 3, tel.EdgeFirings["stall"]["gated"])
	require.Equal(t, 1, tel.EdgeFirings["burn-watch"]["suppressed"])
	require.Equal(t, 1, tel.EdgeFirings["verification"]["cancelled"])

	require.Nil(t, coord.SessionTelemetry("other").EdgeFirings)
}

func TestStallReplanHandoff(t *testing.T) {
	t.Parallel()

	t.Run("replan prompt carries the write set", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		a.ambiguityClarification = true
		result := stallResult()
		// A write before the repeated tail lands in the payload.
		result.Steps = append([]fantasy.StepResult{
			stepWith(fantasy.FinishReasonToolCalls,
				fantasy.ToolCallContent{
					ToolCallID: "tc-w", ToolName: "edit",
					Input: `{"file_path":"internal/x.go"}`,
				},
				fantasy.ToolResultContent{
					ToolCallID: "tc-w", ToolName: "edit",
					Result: fantasy.ToolResultOutputContentText{Text: "ok"},
				},
			),
		}, result.Steps...)
		queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: result, stalled: true})
		require.True(t, queued)
		q, _ := a.messageQueue.Get(sessionID)
		require.Contains(t, q[0].Prompt, "Files written before the stall:")
		require.Contains(t, q[0].Prompt, "internal/x.go")
	})

	t.Run("no writes renders no write set section", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		a.ambiguityClarification = true
		queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID},
			edgeInput{result: stallResult(), stalled: true})
		require.True(t, queued)
		q, _ := a.messageQueue.Get(sessionID)
		require.NotContains(t, q[0].Prompt, "Files written")
	})
}

// failingCountStore fails the user-message count but delegates the
// insert — the turnSeq=0 collapse path.
type failingCountStore struct {
	q *db.Queries
}

func (f failingCountStore) CountUserMessagesBySession(context.Context, string) (int64, error) {
	return 0, fmt.Errorf("count failed")
}

func (f failingCountStore) InsertEdgeFiring(ctx context.Context, p db.InsertEdgeFiringParams) (int64, error) {
	return f.q.InsertEdgeFiring(ctx, p)
}

func TestEdgeFiringTurnSeqFallback(t *testing.T) {
	t.Parallel()

	a, conn, sessionID := newEdgeTestAgent(t, &config.Config{})
	a.edgeStore = failingCountStore{q: db.New(conn)}
	a.ambiguityClarification = true
	sess, err := a.sessions.Get(t.Context(), sessionID)
	require.NoError(t, err)
	sess.Todos = []session.PlanItem{{Content: "open", Status: session.PlanItemPending}}
	_, err = a.sessions.Save(t.Context(), sess)
	require.NoError(t, err)

	// Two boundaries with distinct run stamps must not collapse into
	// one row even though the count failed both times.
	for stamp := uint64(10); stamp <= 11; stamp++ {
		require.True(t, runEdgesForTest(a, t.Context(),
			SessionAgentCall{SessionID: sessionID, RunStamp: stamp},
			edgeInput{result: cleanResult(), turnSeq: 0}))
	}
	var turnSeqs []int64
	rows, err := conn.QueryContext(t.Context(),
		`SELECT turn_seq FROM edge_firings WHERE session_id = ? AND edge = 'todos' ORDER BY turn_seq`, sessionID)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var ts int64
		require.NoError(t, rows.Scan(&ts))
		turnSeqs = append(turnSeqs, ts)
	}
	require.NoError(t, rows.Err())
	require.Len(t, turnSeqs, 2,
		"each boundary keys off its run stamp when the count fails")
	require.NotEqual(t, turnSeqs[0], turnSeqs[1])
	for _, ts := range turnSeqs {
		require.Negative(t, ts, "fallback keys stay out of the real ordinal range")
	}
}

func TestStallReplanHandoffCheckpoint(t *testing.T) {
	t.Parallel()

	a, conn, sessionID := newEdgeTestAgent(t, &config.Config{})
	a.ambiguityClarification = true
	a.notebook = notebook.NewService(db.New(conn), nil, notebook.Options{})
	q := db.New(conn)
	entryID := uuid.New().String()
	_, err := q.CreateNotebookEntry(t.Context(), db.CreateNotebookEntryParams{
		ID:         entryID,
		SessionID:  sessionID,
		TurnNumber: 1,
		EventType:  "checkpoint",
		Title:      "Session checkpoint",
		// The compressed text loses detail the full text keeps.
		EntryText:     "compressed: edited auth.go",
		EntryTextFull: sql.NullString{String: "full: edited auth.go then verified", Valid: true},
		CreatedAt:     time.Now().Unix(),
	})
	require.NoError(t, err)
	require.NoError(t, q.CreateNotebookTag(t.Context(), db.CreateNotebookTagParams{
		EntryID: entryID,
		Tag:     "granularity:boundary",
	}))

	queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID},
		edgeInput{result: stallResult(), stalled: true})
	require.True(t, queued)
	mq, _ := a.messageQueue.Get(sessionID)
	require.Contains(t, mq[0].Prompt, "Latest session checkpoint:")
	require.Contains(t, mq[0].Prompt, "full: edited auth.go then verified",
		"the replan payload prefers the uncompressed checkpoint text")
}

func TestRelTouchedDirs(t *testing.T) {
	t.Parallel()

	wd := "/repo"
	mk := func(tool, input string) fantasy.StepResult {
		id := fmt.Sprintf("tc-%s", tool)
		return stepWith(fantasy.FinishReasonToolCalls,
			fantasy.ToolCallContent{ToolCallID: id, ToolName: tool, Input: input},
			fantasy.ToolResultContent{
				ToolCallID: id, ToolName: tool,
				Result: fantasy.ToolResultOutputContentText{Text: "ok"},
			},
		)
	}
	dirs := relTouchedDirs(wd, []fantasy.StepResult{
		mk("edit", `{"file_path":"/repo/internal/x.go"}`),
		mk("view", `{"file_path":"pkg/y.go"}`),
		mk("view", `{"file_path":"/etc/passwd"}`),
		mk("bash", `{"command":"ls"}`),
	})
	require.Equal(t, []string{"internal", "pkg"}, dirs,
		"absolute dirs under the root relativize; escapes and non-file calls drop")
}

func TestStallEscalateBeatsBurnWatch(t *testing.T) {
	t.Parallel()

	a, conn, sessionID := interactiveEdgeAgent(t)
	// A clean-stopping, write-less, over-threshold run whose last
	// window saturates one signature — the masked-stall fallback and
	// burn-watch both produce escalation-family triggers.
	result := burnResult(burnWatchStepsThreshold+1, 0)
	for i := len(result.Steps) - loopDetectionWindowSize; i < len(result.Steps)-1; i++ {
		id := fmt.Sprintf("tc-s%d", i)
		result.Steps[i] = stepWith(fantasy.FinishReasonToolCalls,
			fantasy.ToolCallContent{
				ToolCallID: id, ToolName: "view", Input: `{"file_path":"same.go"}`,
			},
			fantasy.ToolResultContent{
				ToolCallID: id, ToolName: "view",
				Result: fantasy.ToolResultOutputContentText{Text: "same"},
			},
		)
	}
	queued := runEdgesForTest(a, t.Context(),
		SessionAgentCall{SessionID: sessionID, RepairAttempts: 1},
		edgeInput{result: result, turnSeq: 1})
	require.True(t, queued)
	q, _ := a.messageQueue.Get(sessionID)
	require.Len(t, q, 1)
	require.True(t, strings.HasPrefix(q[0].Prompt, stallRetryPrefix),
		"first-in-runEdgeSet escalation wins the slot")
	require.NotContains(t, q[0].Prompt, burnWatchPrefix)
	require.Empty(t, q[0].deferred,
		"burn-watch's evidence is step-bound — deferred, not carried")
	outcomes := map[string]string{}
	for _, r := range firingRows(t, conn, sessionID) {
		outcomes[r.edge+"|"+r.variant] = r.outcome
	}
	require.Equal(t, "fired", outcomes["stall|escalate"])
	require.Equal(t, "deferred", outcomes["burn-watch|"])
}

func TestVerificationTodosMerge(t *testing.T) {
	t.Parallel()

	a, conn, sessionID := newEdgeTestAgent(t, &config.Config{})
	sess, err := a.sessions.Get(t.Context(), sessionID)
	require.NoError(t, err)
	sess.Todos = []session.PlanItem{{Content: "open", Status: session.PlanItemPending}}
	_, err = a.sessions.Save(t.Context(), sess)
	require.NoError(t, err)

	result := &fantasy.AgentResult{Steps: []fantasy.StepResult{
		stepWith(fantasy.FinishReasonToolCalls, fantasy.ToolResultContent{
			ToolCallID: "tc-edit", ToolName: "edit",
			Result:         fantasy.ToolResultOutputContentText{Text: "edited"},
			ClientMetadata: `{"verification":[{"check":"verify:build","state":"failed","detail":"exit 1"}]}`,
		}),
		stepWith(fantasy.FinishReasonStop, fantasy.TextContent{Text: "done"}),
	}}
	queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID},
		edgeInput{result: result, turnSeq: 1})
	require.True(t, queued)
	q, _ := a.messageQueue.Get(sessionID)
	require.Len(t, q, 1, "retry-family triggers merge into ONE retry")
	require.True(t, strings.HasPrefix(q[0].Prompt, verificationRetryPrefix))
	require.Contains(t, q[0].Prompt, todosRetryPrefix,
		"intra-retry-family rule is concatenate, not first-wins")
	outcomes := map[string]string{}
	for _, r := range firingRows(t, conn, sessionID) {
		outcomes[r.edge] = r.outcome
	}
	require.Equal(t, "fired", outcomes["verification"])
	require.Equal(t, "fired", outcomes["todos"])
}

func TestEdgeFiringTurnSeqAbsoluteAcrossSummary(t *testing.T) {
	t.Parallel()

	a, conn, sessionID := newEdgeTestAgent(t, &config.Config{})
	msgs := message.NewService(db.New(conn))
	for range 3 {
		_, err := msgs.Create(t.Context(), sessionID, message.CreateMessageParams{
			Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "hi"}},
		})
		require.NoError(t, err)
	}
	// Summarize writes an assistant row flagged is_summary_message —
	// the user-message ordinal must not move.
	_, err := msgs.Create(t.Context(), sessionID, message.CreateMessageParams{
		Role:             message.Assistant,
		IsSummaryMessage: true,
		Parts:            []message.ContentPart{message.TextContent{Text: "summary"}},
	})
	require.NoError(t, err)
	_, err = msgs.Create(t.Context(), sessionID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "hi"}},
	})
	require.NoError(t, err)

	got, err := a.edgeStore.CountUserMessagesBySession(t.Context(), sessionID)
	require.NoError(t, err)
	require.Equal(t, int64(4), got,
		"summary rows don't shift the absolute user-message ordinal")
}

// runEdgesForTest adapts runEdges for tests that only assert the
// queued verdict — the sanitized call return is exercised separately.
func runEdgesForTest(a *sessionAgent, ctx context.Context, call SessionAgentCall, in edgeInput) bool {
	queued, _ := a.runEdges(ctx, call, in)
	return queued
}

func TestRunEdgesReturnsSanitizedCall(t *testing.T) {
	t.Parallel()

	a, _, sessionID := newEdgeTestAgent(t, &config.Config{})
	// A mutating run clears the burn-watch marker, and the deferred
	// carrier is consumed — the returned call carries both mutations
	// so a summarize-continue requeue doesn't resurrect them.
	call := SessionAgentCall{
		SessionID:   sessionID,
		burnWatched: true,
		deferred: []deferredTrigger{{
			edge:    a.runEdgeSet()[1],
			trigger: &edgeTrigger{sessionState: true, plan: []planVerdict{}},
		}},
	}
	writeResult := &fantasy.AgentResult{Steps: []fantasy.StepResult{
		stepWith(fantasy.FinishReasonToolCalls,
			fantasy.ToolCallContent{ToolCallID: "tc-1", ToolName: "edit", Input: `{"file_path":"x.go"}`},
			fantasy.ToolResultContent{
				ToolCallID: "tc-1", ToolName: "edit",
				Result: fantasy.ToolResultOutputContentText{Text: "ok"},
			},
		),
		stepWith(fantasy.FinishReasonStop, fantasy.TextContent{Text: "done"}),
	}}
	_, sanitized := a.runEdges(t.Context(), call, edgeInput{result: writeResult})
	require.False(t, sanitized.burnWatched,
		"a writing run clears the marker on the caller's call too")
	require.Nil(t, sanitized.deferred,
		"consumed carrier entries must not re-merge on a requeued call")
	require.True(t, call.burnWatched,
		"the input call is a copy — mutation propagates via the return")
}

func TestStallEdge_SkipsStopTurnBoundary(t *testing.T) {
	t.Parallel()

	a, _, sessionID := newEdgeTestAgent(t, &config.Config{})
	a.ambiguityClarification = true
	// A saturated signature window ending on a question StopTurn is
	// an escalation pause, not a loop stop — the fallback must not
	// produce a replan.
	result := stallResult()
	result.Steps[len(result.Steps)-1] = stepWith(fantasy.FinishReasonToolCalls,
		fantasy.ToolCallContent{ToolCallID: "tc-q", ToolName: "question", Input: `{}`},
		fantasy.ToolResultContent{
			ToolCallID: "tc-q", ToolName: "question",
			StopTurn: true,
			Result:   fantasy.ToolResultOutputContentText{Text: "asked"},
		},
	)
	queued := runEdgesForTest(a, t.Context(), SessionAgentCall{SessionID: sessionID},
		edgeInput{result: result})
	require.False(t, queued)
	q, _ := a.messageQueue.Get(sessionID)
	require.Empty(t, q)
}

func TestEdgeFiringDelta(t *testing.T) {
	t.Parallel()

	sa, _, sessionID := newEdgeTestAgent(t, &config.Config{})
	coord := &coordinator{
		mainAgent:         sa,
		edgeFiringEmitted: csync.NewMap[string, map[string]int](),
	}
	sa.edgeStats.Set(sessionID, map[string]int{"stall:fired": 2, "todos:cleared": 1})
	require.Equal(t, map[string]map[string]int{
		"stall": {"fired": 2}, "todos": {"cleared": 1},
	}, coord.EdgeFiringDelta(sessionID))
	// A second emission reports only the increment — the driver's
	// per-turn summation can't double-count cumulative counters.
	sa.edgeStats.Set(sessionID, map[string]int{"stall:fired": 3, "todos:cleared": 1})
	require.Equal(t, map[string]map[string]int{
		"stall": {"fired": 1},
	}, coord.EdgeFiringDelta(sessionID))
	require.Nil(t, coord.EdgeFiringDelta(sessionID), "no new rows, no delta")
}
