package agent

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/notebook"
	"github.com/stretchr/testify/require"
)

// renderedCall returns the rendered tool-call part for id.
func renderedCall(t *testing.T, history []fantasy.Message, id string) fantasy.ToolCallPart {
	t.Helper()
	for _, m := range history {
		for _, p := range m.Content {
			if tc, ok := p.(fantasy.ToolCallPart); ok && tc.ToolCallID == id {
				return tc
			}
		}
	}
	t.Fatalf("tool call %s missing from rendered history", id)
	return fantasy.ToolCallPart{}
}

// renderedResultText returns the rendered text (or error text) of the
// tool result paired with call id.
func renderedResultText(t *testing.T, history []fantasy.Message, id string) string {
	t.Helper()
	for _, m := range history {
		for _, p := range m.Content {
			tr, ok := p.(fantasy.ToolResultPart)
			if !ok || tr.ToolCallID != id {
				continue
			}
			switch out := tr.Output.(type) {
			case fantasy.ToolResultOutputContentText:
				return out.Text
			case fantasy.ToolResultOutputContentError:
				return out.Error.Error()
			}
		}
	}
	t.Fatalf("tool result %s missing from rendered history", id)
	return ""
}

// renderedText concatenates every rendered text part.
func renderedText(history []fantasy.Message) string {
	var out string
	for _, m := range history {
		for _, p := range m.Content {
			if tp, ok := p.(fantasy.TextPart); ok {
				out += tp.Text + "\n"
			}
		}
	}
	return out
}

// renderedReasoning concatenates every rendered reasoning part.
func renderedReasoning(history []fantasy.Message) string {
	var out string
	for _, m := range history {
		for _, p := range m.Content {
			if rp, ok := p.(fantasy.ReasoningPart); ok {
				out += rp.Text + "\n"
			}
		}
	}
	return out
}

// failEntryGen keeps segments unprocessed forever — the persistent
// small-model outage the collapse gate must degrade through.
type failEntryGen struct{}

func (failEntryGen) Generate(context.Context, string, []notebook.EntryInput) ([]notebook.GeneratedEntry, error) {
	return nil, errors.New("generation failed")
}

func (failEntryGen) GenerateCheckpoint(context.Context, string, string) (notebook.GeneratedEntry, error) {
	return notebook.GeneratedEntry{}, errors.New("generation failed")
}

// priorTurnFixture stores turn 0 — a user prompt, an assistant step
// with reasoning plus a bash pair and a question pair, and a closing
// answer — followed by the next turn's user message.
func priorTurnFixture(t *testing.T, svc message.Service, sessionID string) []message.Message {
	t.Helper()
	mkMsg(t, svc, sessionID, message.User, message.TextContent{Text: "first prompt"})
	mkMsg(t, svc, sessionID, message.Assistant,
		message.TextContent{Text: "investigating"},
		message.ReasoningContent{Thinking: "deep thoughts", ThoughtSignature: "sig", ToolID: "tc-bash"},
		message.ToolCall{ID: "tc-bash", Name: "bash", Input: `{"command":"cat big.go"}`, Finished: true},
		message.ToolCall{ID: "tc-q", Name: "question", Input: `{"questions":[{"type":"yes_no","question":"proceed?"}]}`, Finished: true},
	)
	mkMsg(t, svc, sessionID, message.Tool,
		message.ToolResult{ToolCallID: "tc-bash", Name: "bash", Content: bigContent()},
		message.ToolResult{ToolCallID: "tc-q", Name: "question", Content: "yes — proceed"},
	)
	mkMsg(t, svc, sessionID, message.Assistant, message.TextContent{Text: "turn zero answer"})
	mkMsg(t, svc, sessionID, message.User, message.TextContent{Text: "second prompt"})
	msgs, err := svc.List(t.Context(), sessionID)
	require.NoError(t, err)
	return msgs
}

func TestPreparePrompt_CollapsesCoveredPriorTurn(t *testing.T) {
	t.Parallel()

	a, svc, _, sessionID := newSegmentTestAgent(t, echoEntryGen{})
	a.priorTurns = priorTurnsStub
	msgs := priorTurnFixture(t, svc, sessionID)
	ctx := t.Context()
	// First pass fires coverage; the render's own pass observes the
	// committed rows.
	a.detectSegments(ctx, sessionID, msgs)

	// A run starting turn 1: only turn 0 is eligible for collapse.
	collapse := a.newTurnCollapse(1)
	history, _ := a.preparePrompt(ctx, msgs, false, collapse)

	// Call side: still a valid JSON object, never prose.
	call := renderedCall(t, history, "tc-bash")
	require.JSONEq(t, `{"_collapsed":"prior turn 0"}`, call.Input)
	require.Equal(t, "bash", call.ToolName)

	// Result side: constant stub text naming the turn and the recall
	// pointer — never claiming a digest exists.
	res := renderedResultText(t, history, "tc-bash")
	require.Contains(t, res, "prior turn 0")
	require.Contains(t, res, `recall("result:tc-bash")`)
	require.NotContains(t, res, "file content line")

	// Prior-turn reasoning — including its thought signature — drops
	// with the turn.
	require.NotContains(t, renderedReasoning(history), "deep thoughts")

	// The conversation plane stays verbatim: user messages, assistant
	// text, and the question pair.
	text := renderedText(history)
	require.Contains(t, text, "first prompt")
	require.Contains(t, text, "second prompt")
	require.Contains(t, text, "turn zero answer")
	require.JSONEq(t, `{"questions":[{"type":"yes_no","question":"proceed?"}]}`,
		renderedCall(t, history, "tc-q").Input)
	require.Equal(t, "yes — proceed", renderedResultText(t, history, "tc-q"))

	// Stored events are untouched — the recall pointer can resolve.
	stored, err := svc.List(ctx, sessionID)
	require.NoError(t, err)
	var original string
	for _, m := range stored {
		for _, tr := range m.ToolResults() {
			if tr.ToolCallID == "tc-bash" {
				original = tr.Content
			}
		}
	}
	require.Contains(t, original, "file content line")
}

func TestPreparePrompt_UncoveredPriorTurnStaysRaw(t *testing.T) {
	t.Parallel()

	// Generation always fails — the outage path must degrade to
	// verbatim, never to stubs with no entries behind them.
	a, svc, _, sessionID := newSegmentTestAgent(t, failEntryGen{})
	a.priorTurns = priorTurnsStub
	msgs := priorTurnFixture(t, svc, sessionID)

	history, _ := a.preparePrompt(t.Context(), msgs, false, a.newTurnCollapse(1))
	require.Equal(t, `{"command":"cat big.go"}`, renderedCall(t, history, "tc-bash").Input)
	require.Contains(t, renderedResultText(t, history, "tc-bash"), "file content line")
	require.Contains(t, renderedReasoning(history), "deep thoughts")
}

func TestPreparePrompt_CollapseSetFrozenForRun(t *testing.T) {
	t.Parallel()

	a, svc, _, sessionID := newSegmentTestAgent(t, echoEntryGen{})
	a.priorTurns = priorTurnsStub
	msgs := priorTurnFixture(t, svc, sessionID)
	ctx := t.Context()

	collapse := a.newTurnCollapse(1)
	// The first render fires coverage but reads the registry pre-commit,
	// so turn 0 resolves uncovered — and the set freezes that way.
	h1, _ := a.preparePrompt(ctx, msgs, false, collapse)
	require.Equal(t, `{"command":"cat big.go"}`, renderedCall(t, h1, "tc-bash").Input)

	// Coverage has committed by now, but the frozen set keeps the run's
	// renders byte-stable — no mid-window flip from raw to stub.
	h2, _ := a.preparePrompt(ctx, msgs, false, collapse)
	require.Equal(t, `{"command":"cat big.go"}`, renderedCall(t, h2, "tc-bash").Input)

	// A fresh pipeline — the next run — sees the committed coverage.
	h3, _ := a.preparePrompt(ctx, msgs, false, a.newTurnCollapse(1))
	require.JSONEq(t, `{"_collapsed":"prior turn 0"}`, renderedCall(t, h3, "tc-bash").Input)
}

func TestPreparePrompt_MidRunFoldCannotCollapseRunEvents(t *testing.T) {
	t.Parallel()

	a, svc, _, sessionID := newSegmentTestAgent(t, echoEntryGen{})
	a.priorTurns = priorTurnsStub
	msgs := priorTurnFixture(t, svc, sessionID)
	ctx := t.Context()
	a.detectSegments(ctx, sessionID, msgs)

	collapse := a.newTurnCollapse(1)
	history, _ := a.preparePrompt(ctx, msgs, false, collapse)
	require.JSONEq(t, `{"_collapsed":"prior turn 0"}`, renderedCall(t, history, "tc-bash").Input)

	// The active run produces its own tool pair (turn 1), then a queued
	// prompt folds mid-run — a new user message that advances the
	// positional turn count to 2 under the same run.
	mkMsg(t, svc, sessionID, message.Assistant,
		message.ToolCall{ID: "tc-live", Name: "bash", Input: `{"command":"ls"}`, Finished: true})
	mkMsg(t, svc, sessionID, message.Tool,
		message.ToolResult{ToolCallID: "tc-live", Name: "bash", Content: "live output"})
	mkMsg(t, svc, sessionID, message.User, message.TextContent{Text: "folded follow-up"})
	mkMsg(t, svc, sessionID, message.Assistant,
		message.ToolCall{ID: "tc-folded", Name: "bash", Input: `{"command":"pwd"}`, Finished: true})
	mkMsg(t, svc, sessionID, message.Tool,
		message.ToolResult{ToolCallID: "tc-folded", Name: "bash", Content: "folded output"})
	msgs, err := svc.List(ctx, sessionID)
	require.NoError(t, err)

	// The rebuild uses the run's frozen collapse state — the predicate
	// compares against the run-start turn, not the advanced count, so
	// neither the pre-fold nor the post-fold pair can collapse.
	history, _ = a.preparePrompt(ctx, msgs, false, collapse)
	require.Equal(t, `{"command":"ls"}`, renderedCall(t, history, "tc-live").Input)
	require.Equal(t, `{"command":"pwd"}`, renderedCall(t, history, "tc-folded").Input)
	require.Equal(t, "live output", renderedResultText(t, history, "tc-live"))
	require.JSONEq(t, `{"_collapsed":"prior turn 0"}`, renderedCall(t, history, "tc-bash").Input)
}

func TestPreparePrompt_CollapsedTurnOrphanCallKeepsPairing(t *testing.T) {
	t.Parallel()

	a, svc, _, sessionID := newSegmentTestAgent(t, echoEntryGen{})
	a.priorTurns = priorTurnsStub
	mkMsg(t, svc, sessionID, message.User, message.TextContent{Text: "go"})
	mkMsg(t, svc, sessionID, message.Assistant,
		message.ToolCall{ID: "tc-orph", Name: "bash", Input: `{"command":"sleep 99"}`, Finished: false})
	// The run was interrupted — no result ever landed for the call.
	mkMsg(t, svc, sessionID, message.User, message.TextContent{Text: "next"})
	msgs, err := svc.List(t.Context(), sessionID)
	require.NoError(t, err)

	ctx := t.Context()
	a.detectSegments(ctx, sessionID, msgs)
	history, _ := a.preparePrompt(ctx, msgs, false, a.newTurnCollapse(1))

	call := renderedCall(t, history, "tc-orph")
	require.JSONEq(t, `{"_collapsed":"prior turn 0"}`, call.Input)

	// The synthetic interrupted-result still pairs with the collapsed
	// call — strict-adjacency providers keep a valid wire shape.
	for i, m := range history {
		for _, p := range m.Content {
			tc, ok := p.(fantasy.ToolCallPart)
			if !ok || tc.ToolCallID != "tc-orph" {
				continue
			}
			require.Less(t, i+1, len(history))
			next := history[i+1]
			require.Equal(t, fantasy.MessageRoleTool, next.Role)
			require.NotEmpty(t, renderedResultText(t, history[i:i+2], "tc-orph"))
		}
	}
}

func TestPreparePrompt_OpenTailCoverageForSummarize(t *testing.T) {
	t.Parallel()

	a, svc, _, sessionID := newSegmentTestAgent(t, echoEntryGen{})
	a.priorTurns = priorTurnsStub
	// A single-turn session: the tail is still open — it closes only
	// when the next user message lands — but run-end generation has
	// already committed its coverage.
	mkMsg(t, svc, sessionID, message.User, message.TextContent{Text: "only prompt"})
	mkMsg(t, svc, sessionID, message.Assistant,
		message.ToolCall{ID: "tc-0", Name: "bash", Input: `{"command":"cat big.go"}`, Finished: true})
	mkMsg(t, svc, sessionID, message.Tool,
		message.ToolResult{ToolCallID: "tc-0", Name: "bash", Content: bigContent()})
	msgs, err := svc.List(t.Context(), sessionID)
	require.NoError(t, err)

	segs := segmentBoundaries(msgs, a.segTokenBudget(), a.segMaxSteps())
	require.True(t, segs[len(segs)-1].open, "tail must still be open for this scenario")

	ctx := t.Context()
	a.generateRunEndSegments(ctx, sessionID, msgs, 0, msgs[1].ID)

	// The Summarize call site: no active run, so every completed turn
	// is prior — the horizon sits one past the last user turn.
	collapse := a.newTurnCollapse(int64(countUserMessages(msgs)))
	history, _ := a.preparePrompt(ctx, msgs, false, collapse)
	require.JSONEq(t, `{"_collapsed":"prior turn 0"}`, renderedCall(t, history, "tc-0").Input)
}

func TestNewTurnCollapse_Gates(t *testing.T) {
	t.Parallel()

	a, _, _, _ := newSegmentTestAgent(t, echoEntryGen{})
	require.Nil(t, a.newTurnCollapse(0), "unset mode must be off")
	a.priorTurns = priorTurnsVerbatim
	require.Nil(t, a.newTurnCollapse(0), "verbatim mode must be off")
	a.priorTurns = priorTurnsStub
	require.NotNil(t, a.newTurnCollapse(0))
	a.notebookEnabled = false
	require.Nil(t, a.newTurnCollapse(0), "notebook off coerces verbatim")
}
