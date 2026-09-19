package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
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

// renderedResult returns the rendered result part paired with call id.
func renderedResult(t *testing.T, history []fantasy.Message, id string) fantasy.ToolResultPart {
	t.Helper()
	for _, m := range history {
		for _, p := range m.Content {
			if tr, ok := p.(fantasy.ToolResultPart); ok && tr.ToolCallID == id {
				return tr
			}
		}
	}
	t.Fatalf("tool result %s missing from rendered history", id)
	return fantasy.ToolResultPart{}
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

func (failEntryGen) GenerateDigest(context.Context, string, string) (notebook.GeneratedEntry, error) {
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
		message.ReasoningContent{Thinking: "signed for question", ThoughtSignature: "sig-q", ToolID: "tc-q"},
		message.ToolCall{ID: "tc-bash", Name: "bash", Input: `{"command":"cat big.go"}`, Finished: true},
		message.ToolCall{ID: "tc-q", Name: "question", Input: `{"questions":[{"type":"yes_no","question":"proceed?"}]}`, Finished: true},
		message.ToolCall{ID: "tc-web", Name: "web_search", Input: `{"query":"golang generics"}`, ProviderExecuted: true, Finished: true},
		message.ToolCall{ID: "tc-shot", Name: "view", Input: `{"file_path":"shot.png"}`, Finished: true},
	)
	mkMsg(t, svc, sessionID, message.Tool,
		message.ToolResult{ToolCallID: "tc-bash", Name: "bash", Content: bigContent(), IsError: true},
		message.ToolResult{ToolCallID: "tc-q", Name: "question", Content: "yes — proceed"},
		message.ToolResult{ToolCallID: "tc-web", Name: "web_search", Content: "search result payload"},
		message.ToolResult{ToolCallID: "tc-shot", Name: "view", Content: "screenshot", Data: "aGVsbG8=", MIMEType: "image/png"},
	)
	// A reasoning-only assistant message: collapse must drop it whole
	// rather than emit an empty bubble.
	mkMsg(t, svc, sessionID, message.Assistant,
		message.ReasoningContent{Thinking: "unsigned musing"})
	mkMsg(t, svc, sessionID, message.Assistant, message.TextContent{Text: "turn zero answer"})
	mkMsg(t, svc, sessionID, message.User, message.TextContent{Text: "second prompt"})
	msgs, err := svc.List(t.Context(), sessionID)
	require.NoError(t, err)
	return msgs
}

// A runtime disabled_tools: [recall] strips the recovery path the
// collapse stubs point at — the render must degrade to verbatim rather
// than emit recall("result:<id>") pointers to a tool the agent lacks.
func TestPreparePrompt_NoCollapseWithoutRecall(t *testing.T) {
	t.Parallel()

	a, svc, _, sessionID := newSegmentTestAgent(t, echoEntryGen{})
	a.priorTurns = priorTurnsStub
	// The fixture seeds recall+search; strip both like a live reload
	// disabling them.
	a.tools = csync.NewSliceFrom([]fantasy.AgentTool{&fakeTool{name: "view"}})
	msgs := priorTurnFixture(t, svc, sessionID)
	ctx := t.Context()
	a.detectSegments(ctx, sessionID, msgs)

	collapse := a.newTurnCollapse(1)
	history, _ := a.preparePrompt(ctx, msgs, false, collapse)

	// Pin eligibility: turn 0 IS covered — the verbatim render must
	// come from the recall gate, not an empty collapse set.
	require.NotEmpty(t, collapse.Set)
	res := renderedResultText(t, history, "tc-bash")
	require.NotContains(t, res, `recall("result:`)
	require.Contains(t, res, "file content line")
	call := renderedCall(t, history, "tc-bash")
	require.NotContains(t, call.Input, "_collapsed")
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

	// The stub is informational — the replaced result's IsError flag
	// must not render it as an error.
	require.IsType(t, fantasy.ToolResultOutputContentText{},
		renderedResult(t, history, "tc-bash").Output)

	// A media result collapses to the text stub — the base64 payload
	// must not leak through as ToolResultOutputContentMedia.
	require.IsType(t, fantasy.ToolResultOutputContentText{},
		renderedResult(t, history, "tc-shot").Output)

	// A reasoning-only assistant message in a collapsed turn emits no
	// empty bubble.
	for _, m := range history {
		if m.Role == fantasy.MessageRoleAssistant {
			require.NotEmpty(t, m.Content)
		}
	}
	require.NotContains(t, renderedReasoning(history), "unsigned musing")

	// Reasoning bound to a collapsed call drops — its signature is
	// invalid against the mutated input anyway. Reasoning bound to the
	// exempt question call keeps both thinking and signature: replaying
	// a signed call without its thought signature is a rejection mode.
	reasoning := renderedReasoning(history)
	require.NotContains(t, reasoning, "deep thoughts")
	require.Contains(t, reasoning, "signed for question")

	// Provider-executed calls stay verbatim — typed server-tool inputs
	// may be schema-validated on replay.
	require.JSONEq(t, `{"query":"golang generics"}`, renderedCall(t, history, "tc-web").Input)
	require.Equal(t, "search result payload", renderedResultText(t, history, "tc-web"))

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

func TestPreparePrompt_OpenTailCoverage(t *testing.T) {
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

	// No active run, so every completed turn is prior — the horizon
	// sits one past the last user turn.
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

func TestBuildAgent_PriorTurnsCoercesWhenRecallDisabled(t *testing.T) {
	// disabled_tools: [recall] removes the recovery path the stubs
	// advertise — collapse (and supersession stubbing, same pointer)
	// must coerce off even with the option set.
	coord := newSummaryTestCoordinator(t, `{
  "options": {"disable_default_providers": true, "disable_provider_auto_update": true,
    "notebook_prior_turns": "stub", "notebook_stub_superseded": true,
    "disabled_tools": ["recall"]},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`)
	a, ok := coord.agents[config.AgentCoder].(*sessionAgent)
	require.True(t, ok)
	require.Equal(t, "verbatim", a.priorTurns)
	require.False(t, a.stubSuperseded)

	// Recall present → stub mode flows through.
	coord = newSummaryTestCoordinator(t, `{
  "options": {"disable_default_providers": true, "disable_provider_auto_update": true,
    "notebook_prior_turns": "stub"},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`)
	a, ok = coord.agents[config.AgentCoder].(*sessionAgent)
	require.True(t, ok)
	require.Equal(t, "stub", a.priorTurns)
}

func TestPreparePrompt_WriteClassStubText(t *testing.T) {
	t.Parallel()

	a, svc, _, sessionID := newSegmentTestAgent(t, echoEntryGen{})
	a.priorTurns = priorTurnsStub
	mkMsg(t, svc, sessionID, message.User, message.TextContent{Text: "first"})
	mkMsg(t, svc, sessionID, message.Assistant,
		message.ToolCall{ID: "tc-edit", Name: "edit", Input: `{"file_path":"a.go","old_string":"x","new_string":"y"}`, Finished: true},
		message.ToolCall{ID: "tc-view", Name: "view", Input: `{"file_path":"a.go"}`, Finished: true},
		message.ToolCall{ID: "tc-rm", Name: "bash", Input: `{"command":"rm tmp.txt"}`, Finished: true})
	mkMsg(t, svc, sessionID, message.Tool,
		message.ToolResult{ToolCallID: "tc-edit", Name: "edit", Content: "edited a.go"},
		message.ToolResult{ToolCallID: "tc-view", Name: "view", Content: "package a"},
		message.ToolResult{ToolCallID: "tc-rm", Name: "bash", Content: "removed tmp.txt"})
	mkMsg(t, svc, sessionID, message.User, message.TextContent{Text: "second"})
	msgs, err := svc.List(t.Context(), sessionID)
	require.NoError(t, err)

	ctx := t.Context()
	a.detectSegments(ctx, sessionID, msgs)
	history, _ := a.preparePrompt(ctx, msgs, false, a.newTurnCollapse(1))

	// File-write calls: the payload was the input itself — result:
	// recall can't recover it — so both stubs lead with re-view; the
	// result stub keeps a qualified recall pointer (the result is
	// still recallable, it just holds only the confirmation).
	require.JSONEq(t, `{"_collapsed":"prior turn 0 — write args dropped; re-view the file to reconstruct"}`,
		renderedCall(t, history, "tc-edit").Input)
	editRes := renderedResultText(t, history, "tc-edit")
	require.Contains(t, editRes, "prior turn 0")
	require.Contains(t, editRes, "re-view the file")
	require.Contains(t, editRes, `recall("result:tc-edit") holds only the confirmation`)

	// Read-class keeps the generic marker and the recall pointer.
	require.JSONEq(t, `{"_collapsed":"prior turn 0"}`, renderedCall(t, history, "tc-view").Input)
	require.Contains(t, renderedResultText(t, history, "tc-view"), `recall("result:tc-view")`)

	// A mutating bash call is mutating but not file-write — there is
	// no single file to re-view, and its output stays recallable, so
	// it keeps the generic marker and pointer.
	require.JSONEq(t, `{"_collapsed":"prior turn 0"}`, renderedCall(t, history, "tc-rm").Input)
	require.Contains(t, renderedResultText(t, history, "tc-rm"), `recall("result:tc-rm")`)
}

func TestRecordCollapsedTurns_PersistsAndDedupes(t *testing.T) {
	t.Parallel()

	a, svc, nb, sessionID := newSegmentTestAgent(t, echoEntryGen{})
	a.priorTurns = priorTurnsStub
	msgs := priorTurnFixture(t, svc, sessionID)
	ctx := t.Context()
	a.detectSegments(ctx, sessionID, msgs)

	history, _ := a.preparePrompt(ctx, msgs, false, a.newTurnCollapse(1))
	require.JSONEq(t, `{"_collapsed":"prior turn 0"}`, renderedCall(t, history, "tc-bash").Input)

	// Turn 0 persisted once; tc-bash + tc-shot collapsed while the
	// exempt question and provider-executed pairs stayed verbatim.
	stats, ok := a.stubStats.Get(sessionID)
	require.True(t, ok)
	require.Equal(t, 1, stats.TurnsCollapsed)
	require.Equal(t, 2, stats.EventsCollapsed)

	// Re-renders collapse the same turn every step — the recorded set
	// and the persisted row keep the counters flat.
	_, _ = a.preparePrompt(ctx, msgs, false, a.newTurnCollapse(1))
	stats, _ = a.stubStats.Get(sessionID)
	require.Equal(t, 1, stats.TurnsCollapsed)
	require.Equal(t, 2, stats.EventsCollapsed)

	// The row itself is the cross-process dedupe: a fresh agent's
	// insert reports nothing new.
	inserted, err := nb.RecordCollapsedTurn(ctx, sessionID, 0, 99)
	require.NoError(t, err)
	require.False(t, inserted)
}

func TestRecordCollapsedTurns_SkipsAllExemptTurn(t *testing.T) {
	t.Parallel()

	a, svc, nb, sessionID := newSegmentTestAgent(t, echoEntryGen{})
	a.priorTurns = priorTurnsStub
	// Turn 0 carries only an exempt call — the turn is covered and
	// eligible, but collapse finds nothing to stub inside it.
	mkMsg(t, svc, sessionID, message.User, message.TextContent{Text: "first"})
	mkMsg(t, svc, sessionID, message.Assistant,
		message.ToolCall{ID: "tc-q", Name: "question", Input: `{"questions":[{"type":"yes_no","question":"go?"}]}`, Finished: true})
	mkMsg(t, svc, sessionID, message.Tool,
		message.ToolResult{ToolCallID: "tc-q", Name: "question", Content: "yes"})
	mkMsg(t, svc, sessionID, message.User, message.TextContent{Text: "second"})
	msgs, err := svc.List(t.Context(), sessionID)
	require.NoError(t, err)

	ctx := t.Context()
	// Commit coverage directly — generation emits no entries for an
	// exempt-only turn, so detectSegments would leave it uncovered.
	segs := segmentBoundaries(msgs, a.segTokenBudget(), a.segMaxSteps())
	covered := make([]notebook.ProcessedSegment, 0, len(segs))
	for _, s := range segs {
		covered = append(covered, notebook.ProcessedSegment{
			TurnNumber:    s.turn,
			SegmentNumber: s.number,
			StartIndex:    int64(s.start),
			EndIndex:      int64(s.end),
			State:         notebook.SegmentProcessed,
		})
	}
	require.NoError(t, nb.MarkSegmentsProcessed(ctx, sessionID, covered))

	history, _ := a.preparePrompt(ctx, msgs, false, a.newTurnCollapse(1))
	// The exempt pair renders verbatim even inside a covered turn.
	require.JSONEq(t, `{"questions":[{"type":"yes_no","question":"go?"}]}`,
		renderedCall(t, history, "tc-q").Input)

	// Nothing collapsed, so nothing records — a covered-but-untouched
	// turn must not inflate the counters.
	stats, _ := a.stubStats.Get(sessionID)
	require.Equal(t, 0, stats.TurnsCollapsed)
	require.Equal(t, 0, stats.EventsCollapsed)
	inserted, err := nb.RecordCollapsedTurn(ctx, sessionID, 0, 0)
	require.NoError(t, err)
	require.True(t, inserted, "turn 0 must not have been recorded")
}

// summarizeCaptureModel records the prompt each Stream call received
// and answers with a minimal summary stream.
type summarizeCaptureModel struct {
	fakeLanguageModel
	prompts []fantasy.Prompt
}

func (m *summarizeCaptureModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.prompts = append(m.prompts, call.Prompt)
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: "summary"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "1"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func TestSummarize_RendersVerbatim(t *testing.T) {
	t.Parallel()

	a, svc, _, sessionID := newSegmentTestAgent(t, echoEntryGen{})
	a.priorTurns = priorTurnsStub
	model := &summarizeCaptureModel{}
	a.largeModel = csync.NewValue(Model{
		Model:    model,
		ModelCfg: config.SelectedModel{Model: "fake-model", Provider: "fake"},
	})
	a.systemPromptPrefix = csync.NewValue("")
	a.activeRequests = csync.NewMap[string, *activeCancel]()
	a.messageQueue = csync.NewMap[string, []SessionAgentCall]()

	msgs := priorTurnFixture(t, svc, sessionID)
	ctx := t.Context()
	a.detectSegments(ctx, sessionID, msgs)

	require.NoError(t, a.Summarize(ctx, sessionID, fantasy.ProviderOptions{}, nil))
	require.NotEmpty(t, model.prompts)

	// The summary's input must be the verbatim transcript: a collapsed
	// turn would render the stub marker instead of the stored content,
	// and the summary seeds the next context window from it.
	var body strings.Builder
	for _, m := range model.prompts[0] {
		for _, p := range m.Content {
			switch part := p.(type) {
			case fantasy.ToolCallPart:
				body.WriteString(part.Input)
			case fantasy.ToolResultPart:
				switch out := part.Output.(type) {
				case fantasy.ToolResultOutputContentText:
					body.WriteString(out.Text)
				case fantasy.ToolResultOutputContentError:
					body.WriteString(out.Error.Error())
				}
			}
		}
	}
	require.Contains(t, body.String(), "file content line")
	require.NotContains(t, body.String(), "_collapsed")
}
