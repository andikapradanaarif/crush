package agent

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// bashVerdictStep builds a step whose bash result carries a completed
// verdict — done + exit_code in the metadata, the shape scanVerification
// records as observed evidence.
func bashVerdictStep(id, command string, exitCode int, output string) fantasy.StepResult {
	return stepWith(fantasy.FinishReasonToolCalls,
		fantasy.ToolCallContent{
			ToolCallID: id, ToolName: "bash",
			Input: fmt.Sprintf(`{"command":%q}`, command),
		},
		fantasy.ToolResultContent{
			ToolCallID:     id,
			ToolName:       "bash",
			ClientMetadata: fmt.Sprintf(`{"done":true,"exit_code":%d}`, exitCode),
			Result:         fantasy.ToolResultOutputContentText{Text: output},
		},
	)
}

func TestEscalationHandoffLines(t *testing.T) {
	t.Parallel()

	tr := &edgeTrigger{
		goal:     "fix the flaky parser test",
		report:   "Stopped: bash repeated without making progress.",
		steps:    verifyLoopResult().Steps,
		writeSet: []string{"internal/parse/lex.go"},
		lastCheck: &observedBash{
			command: "go test ./internal/parse",
			isError: true,
			output:  "--- FAIL: TestLex\nexpected token, got EOF",
		},
	}
	out := escalationHandoffLines(tr)
	require.Contains(t, out, "- Goal: fix the flaky parser test")
	require.Contains(t, out, "wrote: internal/parse/lex.go")
	require.Contains(t, out, "- Tried: edit ×1")
	require.Contains(t, out, "bash ×")
	require.Contains(t, out, "- Verification: `go test ./internal/parse` failed")
	require.Contains(t, out, "tail: --- FAIL: TestLex")
	require.Contains(t, out, "- Blocker: Stopped: bash repeated")
	// The prompt-side section prepends its relay framing.
	require.True(t, strings.HasPrefix(escalationHandoffSection(tr), "Run state for the handoff"))
}

func TestEscalationHandoffLinesSparse(t *testing.T) {
	t.Parallel()

	// A trigger with no pulled evidence still renders the stable
	// progress/verification defaults and omits the empty lines.
	out := escalationHandoffLines(&edgeTrigger{steps: cleanResult().Steps})
	require.NotContains(t, out, "- Goal:")
	require.NotContains(t, out, "- Blocker:")
	require.Contains(t, out, "no file writes")
	require.Contains(t, out, "- Tried: grep ×1")
	require.Contains(t, out, "- Verification: no check ran this run")
}

func TestPullEscalationHandoff(t *testing.T) {
	t.Parallel()

	a, svc, sessionID := newGateTestAgent(t, &config.Config{})
	ctx := t.Context()

	// The transcript: a real prompt, then the harness repair prompt —
	// the pull must skip the repair row and report the user's goal.
	_, err := svc.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "fix the flaky parser test"}},
	})
	require.NoError(t, err)
	_, err = svc.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: stallRetryPrefix + ": the same tool calls repeated"}},
	})
	require.NoError(t, err)

	steps := []fantasy.StepResult{
		bashVerdictStep("tc-b1", "go test ./internal/parse", 1, "--- FAIL: TestLex"),
		bashVerdictStep("tc-b2", "go test ./internal/parse", 1, "--- FAIL: TestLex again"),
	}
	tr := &edgeTrigger{steps: steps}
	a.pullEscalationHandoff(ctx, SessionAgentCall{SessionID: sessionID}, tr)

	require.Equal(t, "fix the flaky parser test", tr.goal)
	require.NotNil(t, tr.lastCheck)
	require.Equal(t, "go test ./internal/parse", tr.lastCheck.command)
	require.True(t, tr.lastCheck.isError)
	require.Equal(t, "--- FAIL: TestLex again", tr.lastCheck.output)
}

func TestPullEscalationHandoffNoCheck(t *testing.T) {
	t.Parallel()

	a, _, sessionID := newGateTestAgent(t, &config.Config{})
	tr := &edgeTrigger{steps: cleanResult().Steps}
	a.pullEscalationHandoff(t.Context(), SessionAgentCall{SessionID: sessionID}, tr)
	// No messages and no verdicts — both fields stay empty rather than
	// fabricating evidence.
	require.Empty(t, tr.goal)
	require.Nil(t, tr.lastCheck)
}

func TestStallEscalatePromptCarriesHandoff(t *testing.T) {
	t.Parallel()

	a, svc, sessionID := newGateTestAgent(t, &config.Config{})
	a.ambiguityClarification = true
	a.interactive = true
	a.tools = csync.NewSliceFrom([]fantasy.AgentTool{
		&fakeTool{name: tools.TodosToolName},
		&fakeTool{name: tools.QuestionToolName},
	})
	_, err := svc.Create(t.Context(), sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "unblock the release build"}},
	})
	require.NoError(t, err)

	queued := runEdgesForTest(a, t.Context(),
		SessionAgentCall{SessionID: sessionID, RepairAttempts: 1},
		edgeInput{result: stallResult(), stalled: true})
	require.True(t, queued)
	q, _ := a.messageQueue.Get(sessionID)
	require.Len(t, q, 1)
	p := q[0].Prompt
	require.True(t, strings.HasPrefix(p, stallRetryPrefix))
	require.Contains(t, p, "- Goal: unblock the release build")
	require.Contains(t, p, "- Tried: view ×")
	require.Contains(t, p, "- Verification: no check ran this run")
	require.Contains(t, p, "- Blocker: Stopped:")
	require.Contains(t, p, "question-tool")
}

func TestStallEscalateDegradeCarriesHandoff(t *testing.T) {
	t.Parallel()

	a, svc, sessionID := newGateTestAgent(t, &config.Config{})
	a.ambiguityClarification = true
	_, err := svc.Create(t.Context(), sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "unblock the release build"}},
	})
	require.NoError(t, err)

	asst := &message.Message{Role: message.Assistant}
	queued := runEdgesForTest(a, t.Context(),
		SessionAgentCall{SessionID: sessionID, NonInteractive: true, RepairAttempts: 1},
		edgeInput{result: stallResult(), currentAssistant: asst, stalled: true})
	require.False(t, queued)
	text := asst.Content().Text
	require.Contains(t, text, "Run state:")
	require.Contains(t, text, "- Goal: unblock the release build")
	require.Contains(t, text, "- Blocker: Stopped:")
}
