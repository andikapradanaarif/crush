package agent

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestEditEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		input     string
		wantPath  string
		wantAdded []string
	}{
		{
			"edit new_string", `{"file_path":"a.go","old_string":"x","new_string":"y\nz"}`,
			"a.go",
			[]string{"y", "z"},
		},
		{
			// Context lines carried over from old_string were not
			// introduced by this edit — they must not pad the
			// survival numerator.
			"edit drops carried context", `{"file_path":"a.go","old_string":"x\nctx","new_string":"y\nctx"}`,
			"a.go",
			[]string{"y"},
		},
		{
			"deletion introduces nothing", `{"file_path":"d.go","old_string":"x","new_string":""}`,
			"d.go",
			nil,
		},
		{
			"write content", `{"file_path":"b.go","content":"line1\n\nline2"}`,
			"b.go",
			[]string{"line1", "line2"},
		},
		{
			"multiedit joins", `{"file_path":"c.go","edits":[{"new_string":"p"},{"new_string":"q"}]}`,
			"c.go",
			[]string{"p", "q"},
		},
		{
			"multiedit subtracts per edit", `{"file_path":"c.go","edits":[{"old_string":"keep","new_string":"keep\np"},{"old_string":"z","new_string":"q\nkeep"}]}`,
			"c.go",
			[]string{"p", "q", "keep"},
		},
		{"malformed", `not json`, "", nil},
		{"no path", `{"new_string":"x"}`, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path, added := editEvidence(tc.input)
			require.Equal(t, tc.wantPath, path)
			require.Equal(t, tc.wantAdded, added)
		})
	}
}

// The turn is the span after a non-harness user message up to the
// next one — mutations and bash calls inside it are the evidence;
// reads and the next prompt's content are not.
func TestExtractTurnOutcome(t *testing.T) {
	t.Parallel()
	a, env := referentAgent(t)
	abs := filepath.Join(env.workingDir, "x.go")

	msgs := []message.Message{
		referentUserMsg("u0", "first turn"),
		{Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{
			ID: "tc-old", Name: tools.EditToolName, Finished: true,
			Input: fmt.Sprintf(`{"file_path":%q,"new_string":"old"}`, "old.go"),
		}}},
		referentUserMsg("u1", "fix it"),
		{Role: message.Assistant, Parts: []message.ContentPart{
			message.ToolCall{
				ID: "tc-view", Name: tools.ViewToolName,
				Input: `{"file_path":"v.go"}`, Finished: true,
			},
			message.ToolCall{
				ID: "tc-edit", Name: tools.EditToolName, Finished: true,
				Input: fmt.Sprintf(`{"file_path":%q,"new_string":"new line"}`, abs),
			},
			// A second edit on the same file joins the fingerprint —
			// deduped, so a repeated line counts once.
			message.ToolCall{
				ID: "tc-edit-dup", Name: tools.EditToolName, Finished: true,
				Input: fmt.Sprintf(`{"file_path":%q,"old_string":"x","new_string":"new line\nanother"}`, abs),
			},
			message.ToolCall{
				ID: "tc-bash", Name: tools.BashToolName,
				Input: `{"command":"go test ./..."}`, Finished: true,
			},
		}},
		// A repair prompt inside the span is not a boundary — its
		// work attributes to the judged turn.
		referentUserMsg("r1", RepairPromptPrefixes[0]+" details"),
		{Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{
			ID: "tc-edit2", Name: tools.WriteToolName, Finished: true,
			Input: `{"file_path":"y.go","content":"y"}`,
		}}},
		referentUserMsg("u2", "next turn starts"),
	}

	o := extractTurnOutcome(t.Context(), a.turnOutcomeDeps(), "s1", msgs, 2)
	require.Equal(t, "u1", o.TurnID, "the turn keys on its boundary message")
	require.ElementsMatch(t,
		[]string{filepath.Join("x.go"), "y.go"}, o.EditedPaths)
	require.Equal(t, []string{"new line", "another"}, o.EditHunks["x.go"],
		"the added-lines fingerprint joins the file's row, deduped")
	require.Equal(t, []string{"y"}, o.EditHunks["y.go"])
	require.Equal(t, []string{"go test ./..."}, o.Commands,
		"bash invocations record in ledger-normal form; reads do not")
}

// The live path records the judged turn at the next prompt with that
// prompt's cue verdict, and re-recording is a no-op.
func TestRecordTurnOutcome(t *testing.T) {
	t.Parallel()
	a, _ := referentAgent(t)
	msgs := []message.Message{
		referentUserMsg("u1", "do the thing"),
		{Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{
			ID: "tc1", Name: tools.WriteToolName, Finished: true,
			Input: `{"file_path":"x.go","content":"v1"}`,
		}}},
	}
	call := SessionAgentCall{SessionID: "s1", Prompt: "thanks"}
	recordTurnOutcome(t.Context(), a.turnOutcomeDeps(), call, msgs)

	has, err := a.cmdlog.HasTurnOutcome(t.Context(), "s1", "u1")
	require.NoError(t, err)
	require.True(t, has)

	// Repair retries and sub-agents record nothing.
	recordTurnOutcome(t.Context(), a.turnOutcomeDeps(), SessionAgentCall{
		SessionID: "s1", Prompt: "retry", RepairAttempts: 1,
	}, msgs)
}

// A prompt folded into an earlier run's queue persists as a real
// boundary (drainQueueForStep) but no per-run pass ever judged the
// turn it closed — the live record's same-session sweep writes its
// row off the stored next prompt. The in-flight prompt itself,
// absent from msgs, still must not mint a row.
func TestRecordTurnOutcome_SweepsFoldedBoundaries(t *testing.T) {
	t.Parallel()
	a, env := referentAgent(t)

	msgs := []message.Message{
		referentUserMsg("u1", "first prompt"),
		{Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{
			ID: "tc1", Name: tools.WriteToolName, Finished: true,
			Input: `{"file_path":"a.go","content":"a"}`,
		}}},
		// Folded mid-run: the boundary that closed the previous turn.
		referentUserMsg("uq", "wrong file"),
		{Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{
			ID: "tc2", Name: tools.WriteToolName, Finished: true,
			Input: `{"file_path":"b.go","content":"b"}`,
		}}},
	}
	call := SessionAgentCall{
		SessionID: "s1", Prompt: "thanks", currentUserMsgID: "u2",
	}
	recordTurnOutcome(t.Context(), a.turnOutcomeDeps(), call, msgs)

	readVerdict := func(turnID string) (string, bool) {
		var nv string
		var has bool
		err := env.conn.QueryRowContext(t.Context(),
			`SELECT next_verdict, settled_at IS NOT NULL FROM turn_outcomes
			 WHERE session_id = 's1' AND turn_id = ?`, turnID).
			Scan(&nv, &has)
		if err != nil {
			return "", false
		}
		return nv, has
	}

	// The judged boundary records with the live prompt's cue.
	nv, _ := readVerdict("uq")
	require.Equal(t, cmdlog.ReferentAccepted, nv)

	// The folded turn records with the verdict the stored next
	// prompt implies — "wrong file" is a revision cue.
	nv, _ = readVerdict("u1")
	require.Equal(t, cmdlog.ReferentRevised, nv,
		"the orphaned turn must exist with its stored-prompt verdict")

	// The in-flight prompt records nothing — it is no closed turn.
	var n int
	require.NoError(t, env.conn.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM turn_outcomes WHERE session_id = 's1' AND turn_id = 'u2'`).
		Scan(&n))
	require.Zero(t, n)

	// Re-running is a no-op: HasTurnOutcome plus the unique key keep
	// the sweep idempotent.
	recordTurnOutcome(t.Context(), a.turnOutcomeDeps(), call, msgs)
	nv, _ = readVerdict("u1")
	require.Equal(t, cmdlog.ReferentRevised, nv)
}

// A first-turn session backfills the stored corpus — the previous
// session's turns and their stored next-prompt verdicts arrive even
// though no in-session turn ever judged them.
func TestBackfillTurnOutcomes(t *testing.T) {
	t.Parallel()
	a, env := referentAgent(t)

	old, err := env.sessions.Create(t.Context(), "old")
	require.NoError(t, err)
	// The corpus admits only sessions with a project-keyed ledger row
	// (sessions carry no project column) — the run is its membership
	// proof, as a real backfilled session's own activity would be.
	env.cmdlog.RecordRun(t.Context(), cmdlog.Run{
		SessionID: old.ID, Command: "ls", CWD: env.workingDir,
		Ran: true, ExitCode: 0,
	})
	var turnIDs []string
	for _, m := range []message.CreateMessageParams{
		{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "add file"}}},
		{Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{
			ID: "tc1", Name: tools.WriteToolName, Finished: true,
			Input: `{"file_path":"a.go","content":"a"}`,
		}}},
		{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "wrong file"}}},
		{Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{
			ID: "tc2", Name: tools.WriteToolName, Finished: true,
			Input: `{"file_path":"b.go","content":"b"}`,
		}}},
	} {
		created, err := env.messages.Create(t.Context(), old.ID, m)
		require.NoError(t, err)
		if m.Role == message.User {
			turnIDs = append(turnIDs, created.ID)
		}
	}

	// First turn of the new session: no prior user message, so the
	// record path becomes the (detached) backfill — poll for it.
	recordTurnOutcome(t.Context(), a.turnOutcomeDeps(),
		SessionAgentCall{SessionID: "s-new", Prompt: "hello"}, nil)

	require.Eventually(t, func() bool {
		has, err := a.cmdlog.HasTurnOutcome(t.Context(), old.ID, turnIDs[0])
		return err == nil && has
	}, 5*time.Second, 10*time.Millisecond, "the old session's first turn backfills")
	require.Eventually(t, func() bool {
		has, err := a.cmdlog.HasTurnOutcome(t.Context(), old.ID, turnIDs[1])
		return err == nil && has
	}, 5*time.Second, 10*time.Millisecond, "the session-final turn backfills too")
}

// The rewritten summary message (Role=User after getSessionMessages
// rewrites it) is not a turn boundary: extraction keys on the real
// user message's ID, and a summary inside a turn's span doesn't
// truncate its work attribution.
func TestExtractTurnOutcome_SummaryIsNotBoundary(t *testing.T) {
	t.Parallel()
	a, env := referentAgent(t)
	abs := filepath.Join(env.workingDir, "x.go")

	msgs := []message.Message{
		referentUserMsg("u0", "do work"),
		{Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{
			ID: "tc-before", Name: tools.EditToolName, Finished: true,
			Input: fmt.Sprintf(`{"file_path":%q,"new_string":"a"}`, abs),
		}}},
		// Mid-span compaction — the summary record sits inside the
		// turn, not before the next real prompt.
		{
			ID: "sum", Role: message.User, IsSummaryMessage: true,
			Parts: []message.ContentPart{message.TextContent{Text: "session so far"}},
		},
		{Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{
			ID: "tc-after", Name: tools.EditToolName, Finished: true,
			Input: fmt.Sprintf(`{"file_path":%q,"new_string":"b"}`, "y.go"),
		}}},
	}

	o := extractTurnOutcome(t.Context(), a.turnOutcomeDeps(), "s1", msgs, 0)
	require.Equal(t, "u0", o.TurnID)
	require.ElementsMatch(t, []string{"x.go", "y.go"}, o.EditedPaths,
		"work on both sides of a compaction belongs to the same turn")
}

// A post-compaction first turn: the tail carries only the summary
// message, so the judged turn lives in the full transcript — the
// live path lists it and still records the row with this prompt's
// verdict.
func TestRecordTurnOutcome_PostCompactionTail(t *testing.T) {
	t.Parallel()
	a, env := referentAgent(t)

	sess, err := env.sessions.Create(t.Context(), "s1")
	require.NoError(t, err)
	var userID string
	for _, m := range []message.CreateMessageParams{
		{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "fix the bug"}}},
		{Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{
			ID: "tc1", Name: tools.WriteToolName, Finished: true,
			Input: `{"file_path":"x.go","content":"v1"}`,
		}}},
	} {
		created, err := env.messages.Create(t.Context(), sess.ID, m)
		require.NoError(t, err)
		if m.Role == message.User {
			userID = created.ID
		}
	}

	// The tail the live path receives: summary only, role rewritten.
	tail := []message.Message{{
		ID: "sum", Role: message.User, IsSummaryMessage: true,
		Parts: []message.ContentPart{message.TextContent{Text: "summary"}},
	}}
	// Production ordering: createUserMessage persists the prompt
	// BEFORE the record pass runs, so the fallback's fresh List sees
	// it — the call carries its ID so the scan can exclude it.
	inflight, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "looks right"}},
	})
	require.NoError(t, err)
	recordTurnOutcome(t.Context(), a.turnOutcomeDeps(), SessionAgentCall{
		SessionID: sess.ID, Prompt: "looks right", currentUserMsgID: inflight.ID,
	}, tail)

	has, err := a.cmdlog.HasTurnOutcome(t.Context(), sess.ID, userID)
	require.NoError(t, err)
	require.True(t, has,
		"the pre-summary judged turn records with this prompt as its verdict")
	has, err = a.cmdlog.HasTurnOutcome(t.Context(), sess.ID, inflight.ID)
	require.NoError(t, err)
	require.False(t, has,
		"the in-flight prompt is not a judged turn — no phantom row")
}

// The in-flight prompt is persisted before the record pass reads the
// transcript — without the exclusion the fallback lands on it and
// mints a phantom row whose key then blocks the real evidence row.
// A first-turn session has no judged turn: no row, only the backfill.
func TestRecordTurnOutcome_InFlightPromptExcluded(t *testing.T) {
	t.Parallel()
	a, env := referentAgent(t)

	sess, err := env.sessions.Create(t.Context(), "s-new")
	require.NoError(t, err)
	inflight, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "first prompt"}},
	})
	require.NoError(t, err)

	recordTurnOutcome(t.Context(), a.turnOutcomeDeps(), SessionAgentCall{
		SessionID: sess.ID, Prompt: "first prompt", currentUserMsgID: inflight.ID,
	}, nil)

	has, err := a.cmdlog.HasTurnOutcome(t.Context(), sess.ID, inflight.ID)
	require.NoError(t, err)
	require.False(t, has,
		"turn 1's own prompt is not a judged turn — the phantom never mints")
}

// A delegated turn's evidence lives in the child session's
// transcript — whose first stored message is the delegation prompt
// (Role=User). Boundary-checking the child slice would return on
// that prompt and record an empty turn; user-role messages inside a
// child are delegation text, not boundaries.
func TestExtractTurnOutcome_DelegatedEdits(t *testing.T) {
	t.Parallel()
	a, env := referentAgent(t)
	abs := filepath.Join(env.workingDir, "delegated.go")

	// Parent transcript: user prompt + an agent tool call.
	msgs := []message.Message{
		referentUserMsg("u1", "delegate the fix"),
		{ID: "a1", Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{
			ID: "tc-agent", Name: AgentToolName,
			Input:    `{"prompt":"fix delegated.go"}`,
			Finished: true,
		}}},
	}

	// Child session transcript as the agent tool actually writes it:
	// delegation prompt first, then the assistant's edit and bash.
	childID := env.sessions.CreateAgentToolSessionID("a1", "tc-agent")
	_, err := env.sessions.CreateTaskSession(t.Context(), childID, "s1", "task")
	require.NoError(t, err)
	for _, m := range []message.CreateMessageParams{
		{Role: message.User, Parts: []message.ContentPart{
			message.TextContent{Text: "fix delegated.go"},
		}},
		{Role: message.Assistant, Parts: []message.ContentPart{
			message.ToolCall{
				ID: "tc-edit", Name: tools.EditToolName, Finished: true,
				Input: fmt.Sprintf(`{"file_path":%q,"new_string":"new"}`, abs),
			},
			message.ToolCall{
				ID: "tc-bash", Name: tools.BashToolName, Finished: true,
				Input: `{"command":"go build ./..."}`,
			},
		}},
	} {
		_, err := env.messages.Create(t.Context(), childID, m)
		require.NoError(t, err)
	}

	o := extractTurnOutcome(t.Context(), a.turnOutcomeDeps(), "s1", msgs, 0)
	require.Equal(t, "u1", o.TurnID)
	require.Equal(t, []string{"delegated.go"}, o.EditedPaths,
		"the delegation prompt inside the child is not a boundary")
	require.Equal(t, []string{"new"}, o.EditHunks["delegated.go"])
	require.Equal(t, []string{"go build ./..."}, o.Commands)
}

// The detached record pass must snapshot every agent field it needs
// on the caller's goroutine, before spawn — tests re-stamp fields
// mid-flight and a detached read of a.messages/a.sessions/a.cmdlog/
// a.configStore races the re-stamp (the sibling settle pass guards
// the same way). This pins the contract: re-stamp while passes are
// in flight; under -race a leaked field read flags immediately.
func TestRecordTurnOutcomeDetachedFieldSnapshot(t *testing.T) {
	t.Parallel()
	a, env := referentAgent(t)
	a.detachedWork = &sync.WaitGroup{}

	msgs := []message.Message{
		referentUserMsg("u1", "do the thing"),
		{Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{
			ID: "tc1", Name: tools.WriteToolName, Finished: true,
			Input: `{"file_path":"x.go","content":"v1"}`,
		}}},
	}
	call := SessionAgentCall{SessionID: "s1", Prompt: "next turn"}
	for range 50 {
		a.recordTurnOutcome(t.Context(), call, msgs)
		// Re-stamp the fields the detached pass used to read —
		// nil first so a late read would also panic, then restore
		// so the next iteration's guard passes.
		a.messages = nil
		a.sessions = nil
		a.cmdlog = nil
		a.configStore = nil
		a.messages = env.messages
		a.sessions = env.sessions
		a.cmdlog = env.cmdlog
		a.configStore = config.NewTestStoreWithDir(&config.Config{}, env.workingDir)
	}
	a.detachedWork.Wait()
}
