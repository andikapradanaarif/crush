package agent

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent/tools"
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
			"write content", `{"file_path":"b.go","content":"line1\n\nline2"}`,
			"b.go",
			[]string{"line1", "line2"},
		},
		{
			"multiedit joins", `{"file_path":"c.go","edits":[{"new_string":"p"},{"new_string":"q"}]}`,
			"c.go",
			[]string{"p", "q"},
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

	o := a.extractTurnOutcome(t.Context(), "s1", msgs, 2)
	require.Equal(t, 1, o.TurnIdx, "u1 is the second non-repair user turn")
	require.ElementsMatch(t,
		[]string{filepath.Join("x.go"), "y.go"}, o.EditedPaths)
	require.Equal(t, []string{"new line"}, o.EditHunks["x.go"],
		"the added-lines fingerprint joins the file's row")
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
	a.recordTurnOutcome(t.Context(), call, msgs)

	has, err := a.cmdlog.HasTurnOutcome(t.Context(), "s1", 0)
	require.NoError(t, err)
	require.True(t, has)

	// Repair retries and sub-agents record nothing.
	a.recordTurnOutcome(t.Context(), SessionAgentCall{
		SessionID: "s1", Prompt: "retry", RepairAttempts: 1,
	}, msgs)
}

// A first-turn session backfills the stored corpus — the previous
// session's turns and their stored next-prompt verdicts arrive even
// though no in-session turn ever judged them.
func TestBackfillTurnOutcomes(t *testing.T) {
	t.Parallel()
	a, env := referentAgent(t)

	old, err := env.sessions.Create(t.Context(), "old")
	require.NoError(t, err)
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
		_, err := env.messages.Create(t.Context(), old.ID, m)
		require.NoError(t, err)
	}

	// First turn of the new session: no prior user message, so the
	// record path becomes the (detached) backfill — poll for it.
	a.recordTurnOutcome(t.Context(),
		SessionAgentCall{SessionID: "s-new", Prompt: "hello"}, nil)

	require.Eventually(t, func() bool {
		has, err := a.cmdlog.HasTurnOutcome(t.Context(), old.ID, 0)
		return err == nil && has
	}, 5*time.Second, 10*time.Millisecond, "the old session's first turn backfills")
	require.Eventually(t, func() bool {
		has, err := a.cmdlog.HasTurnOutcome(t.Context(), old.ID, 1)
		return err == nil && has
	}, 5*time.Second, 10*time.Millisecond, "the session-final turn backfills too")
}
