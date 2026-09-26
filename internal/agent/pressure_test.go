package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	notebooktool "github.com/charmbracelet/crush/internal/agent/tools/notebook"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/notebook"
	"github.com/stretchr/testify/require"
)

// pressureTestAgent builds the minimum sessionAgent the gate touches:
// a model with a declared window/output reserve and a request-stats
// map like the coordinator's shared one.
func pressureTestAgent(cw, maxOut int64) *sessionAgent {
	return &sessionAgent{
		largeModel: csync.NewValue(Model{CatwalkCfg: catwalk.Model{
			ContextWindow:    cw,
			DefaultMaxTokens: maxOut,
		}}),
		reqStats:     csync.NewMap[string, requestStats](),
		pressureGate: true,
	}
}

func TestPressureMargin(t *testing.T) {
	t.Parallel()

	// Window-independent: output reserve plus the bounded step jump.
	// A window-proportional floor was always dominated by this sum
	// and is deliberately absent.
	require.Equal(t, int64(8_000+4*12_500), pressureMargin(8_000))
	require.Equal(t, int64(4_096+4*12_500), pressureMargin(4_096))
	// A large catalog output reserve raises the margin further — the
	// request must leave real generation headroom.
	require.Equal(t, int64(384_000+4*12_500), pressureMargin(384_000))
}

func TestPressureGate_DisengagedBelowMargin(t *testing.T) {
	t.Parallel()

	a := pressureTestAgent(1_000_000, 8_000)
	msgs := []message.Message{segUser("hello"), segAssistant("hi")}

	require.False(t, a.pressureEngaged("s1", msgs))

	rs, ok := a.reqStats.Get("s1")
	require.True(t, ok)
	require.False(t, rs.pressureEngaged)
	require.Zero(t, rs.pressureActivations)
	require.Equal(t, len(msgs), rs.renderedMsgs)
	require.Positive(t, rs.pressureEstimate)
}

func TestPressureGate_ColdStartWholeRenderEstimate(t *testing.T) {
	t.Parallel()

	a := pressureTestAgent(64_000, 8_000)
	// No usage recorded — the estimate is the whole verbatim render.
	// margin = max(64K*0.2, 8K+50K) = 58K → threshold 6K tokens ≈ 24K
	// chars; 40K chars clears it.
	big := segUser(strings.Repeat("x", 40_000))
	require.True(t, a.pressureEngaged("s1", []message.Message{big}))
}

func TestPressureGate_EngagesAcrossThreshold(t *testing.T) {
	t.Parallel()

	a := pressureTestAgent(1_000_000, 8_000)
	msgs := []message.Message{segUser("hello")}
	// threshold = 1M - 58K = 942K. Just below stays verbatim.
	a.reqStats.Set("s1", requestStats{LastPromptTokens: 941_000, renderedMsgs: 1})
	require.False(t, a.pressureEngaged("s1", msgs))

	// At the boundary the gate trips and counts one activation.
	a.reqStats.Set("s1", requestStats{LastPromptTokens: 942_000, renderedMsgs: 1})
	require.True(t, a.pressureEngaged("s1", msgs))
	rs, _ := a.reqStats.Get("s1")
	require.True(t, rs.pressureEngaged)
	require.Equal(t, 1, rs.pressureActivations)
}

func TestPressureGate_LatchesOnceEngaged(t *testing.T) {
	t.Parallel()

	a := pressureTestAgent(1_000_000, 8_000)
	msgs := []message.Message{segUser("hello")}

	a.reqStats.Set("s1", requestStats{LastPromptTokens: 950_000, renderedMsgs: 1})
	require.True(t, a.pressureEngaged("s1", msgs))

	// A lower later estimate must not disengage — the latch is the
	// contract that append-only history can't shrink back under the
	// margin.
	a.reqStats.Set("s1", requestStats{
		LastPromptTokens:    1_000,
		renderedMsgs:        1,
		pressureEngaged:     true,
		pressureActivations: 1,
	})
	require.True(t, a.pressureEngaged("s1", msgs))
	rs, _ := a.reqStats.Get("s1")
	require.Equal(t, 1, rs.pressureActivations, "latch must not count a second activation")
}

func TestPressureGate_WatermarkDelta(t *testing.T) {
	t.Parallel()

	a := pressureTestAgent(1_000_000, 8_000)
	msgs := []message.Message{
		segUser("first"),
		segAssistant("done"),
		segUser(strings.Repeat("y", 8_000)),
	}
	// The first two messages were covered by the last request — only
	// the 8K-char tail adds ~2K tokens over the reported 10K.
	a.reqStats.Set("s1", requestStats{LastPromptTokens: 10_000, renderedMsgs: 2})
	require.False(t, a.pressureEngaged("s1", msgs))

	rs, _ := a.reqStats.Get("s1")
	require.Equal(t, int64(10_000+8_000/4), rs.pressureEstimate)
	require.Equal(t, 3, rs.renderedMsgs)
}

func TestPressureGate_UnknownWindowKeepsMachineryOn(t *testing.T) {
	t.Parallel()

	a := pressureTestAgent(0, 8_000)
	msgs := []message.Message{segUser("hello")}

	// No declared window means no margin to cross — the render
	// machinery stays on (fail-safe default), but nothing latches or
	// counts: "engaged" means a measured crossing, not the default.
	// The estimate and watermark still record so the run audits as
	// measured rather than absent.
	require.True(t, a.pressureEngaged("s1", msgs))
	rs, ok := a.reqStats.Get("s1")
	require.True(t, ok, "unknown window still records the estimate")
	require.Positive(t, rs.pressureEstimate)
	require.False(t, rs.pressureEngaged)
	require.Zero(t, rs.pressureActivations)
}

func TestPressureGate_MissingStatsKeepsMachineryOn(t *testing.T) {
	t.Parallel()

	a := pressureTestAgent(1_000_000, 8_000)
	a.reqStats = nil
	require.True(t, a.pressureEngaged("s1", []message.Message{segUser("hi")}))
	require.True(t, a.pressureEngaged("", []message.Message{segUser("hi")}))
}

func TestPressureGate_LatchSurvivesAgentRebuild(t *testing.T) {
	t.Parallel()

	// Coordinator rebuilds share the request-stats map — a latch
	// recorded by one agent generation must hold for the next.
	shared := csync.NewMap[string, requestStats]()
	mk := func() *sessionAgent {
		a := pressureTestAgent(1_000_000, 8_000)
		a.reqStats = shared
		return a
	}
	msgs := []message.Message{segUser("hello")}

	a1 := mk()
	a1.reqStats.Set("s1", requestStats{LastPromptTokens: 950_000, renderedMsgs: 1})
	require.True(t, a1.pressureEngaged("s1", msgs))

	a2 := mk()
	require.True(t, a2.pressureEngaged("s1", msgs))
	rs, _ := shared.Get("s1")
	require.Equal(t, 1, rs.pressureActivations)
}

// Gate on, comfortable regime: the render stays verbatim and the
// collapse set never freezes — nothing is locked in below pressure.
func TestPreparePrompt_PressureGateDisengagedVerbatim(t *testing.T) {
	t.Parallel()

	a, svc, _, sessionID := newSegmentTestAgent(t, echoEntryGen{})
	a.priorTurns = priorTurnsStub
	a.pressureGate = true
	a.largeModel = csync.NewValue(Model{CatwalkCfg: catwalk.Model{
		ContextWindow:    1_000_000,
		DefaultMaxTokens: 8_000,
	}})
	a.reqStats = csync.NewMap[string, requestStats]()
	msgs := priorTurnFixture(t, svc, sessionID)

	collapse := a.newTurnCollapse(1)
	history, _ := a.preparePrompt(t.Context(), msgs, false, collapse, false)

	require.Nil(t, collapse.Set, "disengaged render must not freeze a collapse set")
	res := renderedResultText(t, history, "tc-bash")
	require.Contains(t, res, "file content line")
	require.NotContains(t, res, `recall("result:`)

	rs, ok := a.reqStats.Get(sessionID)
	require.True(t, ok)
	require.False(t, rs.pressureEngaged)
	require.Zero(t, rs.pressureActivations)
	require.Positive(t, rs.pressureEstimate)
}

// Gate on, pressure regime: the same fixture collapses turn 0 — the
// machinery is the render path the gate switches on.
func TestPreparePrompt_PressureGateEngagedCollapses(t *testing.T) {
	t.Parallel()

	a, svc, _, sessionID := newSegmentTestAgent(t, echoEntryGen{})
	a.priorTurns = priorTurnsStub
	a.pressureGate = true
	// margin (8K reserve + 50K jump) exceeds the 40K window — the
	// gate engages on the first estimate.
	a.largeModel = csync.NewValue(Model{CatwalkCfg: catwalk.Model{
		ContextWindow:    40_000,
		DefaultMaxTokens: 8_000,
	}})
	a.reqStats = csync.NewMap[string, requestStats]()
	msgs := priorTurnFixture(t, svc, sessionID)
	// First pass fires coverage; the render's own pass observes the
	// committed rows.
	a.detectSegments(t.Context(), sessionID, msgs)

	collapse := a.newTurnCollapse(1)
	history, _ := a.preparePrompt(t.Context(), msgs, false, collapse, false)

	require.NotEmpty(t, collapse.Set)
	res := renderedResultText(t, history, "tc-bash")
	require.Contains(t, res, `recall("result:tc-bash")`)
	require.NotContains(t, res, "file content line")

	rs, _ := a.reqStats.Get(sessionID)
	require.True(t, rs.pressureEngaged)
	require.Equal(t, 1, rs.pressureActivations)
}

// Gate off is the pre-gate contract: machinery renders regardless of
// headroom, and no gate state is written.
func TestPreparePrompt_GateOffCollapsesUnconditionally(t *testing.T) {
	t.Parallel()

	a, svc, _, sessionID := newSegmentTestAgent(t, echoEntryGen{})
	a.priorTurns = priorTurnsStub
	// pressureGate stays false; largeModel is never consulted.
	msgs := priorTurnFixture(t, svc, sessionID)
	a.detectSegments(t.Context(), sessionID, msgs)

	collapse := a.newTurnCollapse(1)
	history, _ := a.preparePrompt(t.Context(), msgs, false, collapse, false)

	require.NotEmpty(t, collapse.Set)
	res := renderedResultText(t, history, "tc-bash")
	require.Contains(t, res, `recall("result:tc-bash")`)
}

// The freeze must wait for real coverage: a first render finding no
// covered prior turns leaves collapse.Set nil so a later coverage
// commit can still activate collapse within the same run.
func TestPreparePrompt_EmptyCoverageDefersFreeze(t *testing.T) {
	t.Parallel()

	a, svc, _, sessionID := newSegmentTestAgent(t, echoEntryGen{})
	a.priorTurns = priorTurnsStub
	a.pressureGate = true
	a.largeModel = csync.NewValue(Model{CatwalkCfg: catwalk.Model{
		ContextWindow:    40_000,
		DefaultMaxTokens: 8_000,
	}})
	a.reqStats = csync.NewMap[string, requestStats]()

	// Turn 0 unclosed: store everything up to (not including) the
	// closing user prompt — no completed turn is covered yet.
	msgs := priorTurnFixture(t, svc, sessionID)
	unclosed := msgs[:len(msgs)-1]
	ctx := t.Context()

	collapse := a.newTurnCollapse(1)
	history, _ := a.preparePrompt(ctx, unclosed, false, collapse, false)
	require.Nil(t, collapse.Set, "empty coverage must not freeze the set")

	// The closing user message lands. This render fires turn 0's
	// coverage but reads the registry pre-commit — still uncovered,
	// still unfrozen.
	history, _ = a.preparePrompt(ctx, msgs, false, collapse, false)
	require.Nil(t, collapse.Set)

	// The next render observes the committed coverage — the same
	// run's collapse set activates late rather than never.
	history, _ = a.preparePrompt(ctx, msgs, false, collapse, false)
	require.NotNil(t, collapse.Set)
	require.True(t, collapse.Set[0])
	res := renderedResultText(t, history, "tc-bash")
	require.Contains(t, res, `recall("result:tc-bash")`)
}

// wireText serializes everything the provider would receive — text,
// tool-call inputs, and result outputs — so wire-level assertions can
// see collapse markers, not just text parts.
func wireText(p fantasy.Prompt) string {
	var b strings.Builder
	for _, msg := range p {
		for _, part := range msg.Content {
			if t, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
				b.WriteString(t.Text)
			}
			if tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				b.WriteString(tc.Input)
			}
			if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				if txt, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Output); ok {
					b.WriteString(txt.Text)
				}
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// captureModel records every main-turn request's wire text and answers
// text finishes — optionally emitting one bash tool call first so the
// run has a collapsible pair. Title generation is answered without
// consuming the script.
type captureModel struct {
	fakeLanguageModel
	mu      sync.Mutex
	prompts []string
	tool    bool
	usage   int64
}

func (m *captureModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if strings.Contains(promptText(call.Prompt), "Generate a concise title") {
		return gateTextStream("title"), nil
	}
	m.mu.Lock()
	m.prompts = append(m.prompts, wireText(call.Prompt))
	m.mu.Unlock()
	if m.tool {
		m.tool = false
		input := `{"command":"ls"}`
		return func(yield func(fantasy.StreamPart) bool) {
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: "tc1", ToolCallName: "bash"}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputDelta, ID: "tc1", Delta: input}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: "tc1"}) {
				return
			}
			if !yield(fantasy.StreamPart{
				Type:          fantasy.StreamPartTypeToolCall,
				ID:            "tc1",
				ToolCallName:  "bash",
				ToolCallInput: input,
			}) {
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls, Usage: fantasy.Usage{InputTokens: m.usage, OutputTokens: 5}})
		}, nil
	}
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "t1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: "done"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop, Usage: fantasy.Usage{InputTokens: m.usage, OutputTokens: 5}})
	}, nil
}

// TestRun_PressureGateWireProof is the end-to-end evidence: two real
// Run() "processes" over one session, with the provider-visible
// request captured on the wire. Below the margin the request is
// verbatim and uncounted; once stored history crosses it, the same
// machinery collapses the covered prior turn and the latch records
// exactly one activation.
func TestRun_PressureGateWireProof(t *testing.T) {
	t.Parallel()
	env, sessionID := newProcessEnv(t, &countingGen{})

	// A 64K window with a 4K output reserve → margin ~54K → engage
	// threshold ≈ 10K estimated tokens. RawTokenBudget is large so
	// the boundary stays 0 and collapse renders stubs in place —
	// the clearest wire evidence.
	mk := func(m fantasy.LanguageModel) *sessionAgent {
		model := Model{Model: m, CatwalkCfg: catwalk.Model{ContextWindow: 64_000, DefaultMaxTokens: 4_000}}
		return NewSessionAgent(SessionAgentOptions{
			LargeModel:         model,
			SmallModel:         model,
			SystemPrompt:       "system",
			IsYolo:             true,
			Sessions:           env.sessions,
			Messages:           env.messages,
			Notebook:           env.notebook,
			NotebookEnabled:    true,
			NotebookPriorTurns: priorTurnsStub,
			PressureGate:       true,
			RawTokenBudget:     1_000_000,
			DetachedWork:       &sync.WaitGroup{},
			Tools: []fantasy.AgentTool{
				&fakeTool{name: "bash", resp: fantasy.NewTextResponse("ok")},
				&fakeTool{name: notebooktool.RecallToolName, resp: fantasy.NewTextResponse("recalled")},
				&fakeTool{name: notebooktool.SearchToolName, resp: fantasy.NewTextResponse("")},
			},
		}).(*sessionAgent)
	}

	// Process 1: comfortable regime — a small history stays well
	// under the margin, so the request is verbatim and uncounted.
	m1 := &captureModel{tool: true, usage: 2_000}
	a1 := mk(m1)
	_, err := a1.Run(t.Context(), SessionAgentCall{SessionID: sessionID, Prompt: "turn one"})
	require.NoError(t, err)
	a1.detachedWork.Wait()

	require.NotEmpty(t, m1.prompts)
	wire1 := m1.prompts[len(m1.prompts)-1]
	require.Contains(t, wire1, `{"command":"ls"}`, "the tool pair must reach the wire verbatim below pressure")
	require.NotContains(t, wire1, "_collapsed")
	rs1, ok := a1.reqStats.Get(sessionID)
	require.True(t, ok)
	require.False(t, rs1.pressureEngaged)
	require.Zero(t, rs1.pressureActivations)

	// History grows past the margin between processes — a stored
	// ~200KB message is ~50K tokens at chars/4, well over the ~10K
	// threshold of a 64K window.
	mkMsg(t, env.messages, sessionID, message.User,
		message.TextContent{Text: strings.Repeat("filler ", 30_000)})

	// Process 2: a fresh agent over the same session, like `crush
	// run` — no shared in-memory stats, so the cold-start estimate
	// covers the whole verbatim render and trips immediately.
	m2 := &captureModel{usage: 2_000}
	a2 := mk(m2)
	_, err = a2.Run(t.Context(), SessionAgentCall{SessionID: sessionID, Prompt: "turn two"})
	require.NoError(t, err)
	a2.detachedWork.Wait()

	rs2, ok := a2.reqStats.Get(sessionID)
	require.True(t, ok)
	require.True(t, rs2.pressureEngaged)
	require.Equal(t, 1, rs2.pressureActivations)
	require.NotEmpty(t, rs2.Steps)
	require.True(t, rs2.Steps[0].PressureEngaged, "the per-step row carries the latch")

	require.NotEmpty(t, m2.prompts)
	wire2 := m2.prompts[len(m2.prompts)-1]
	require.Contains(t, wire2, `{"_collapsed":"prior turn 0"}`)
	require.Contains(t, wire2, `recall("result:tc1")`)
	require.Contains(t, wire2, "filler", "the uncovered tail stays verbatim")
	require.NotContains(t, wire2, `{"command":"ls"}`,
		"the covered prior turn's verbatim call must not reach the wire under pressure")
}

// Within one process the gate can flip mid-run: a render below the
// margin is verbatim, history grows, and the next render of the SAME
// collapse pipeline engages and collapses. rebuildStepMessages reuses
// the run's collapse object across renders, so the test shares it too.
func TestPreparePrompt_PressureGateIntraProcessTransition(t *testing.T) {
	t.Parallel()

	a, svc, _, sessionID := newSegmentTestAgent(t, echoEntryGen{})
	a.priorTurns = priorTurnsStub
	a.pressureGate = true
	// 64K window, 4K reserve → margin 54K → engage threshold ≈ 10K
	// estimated tokens ≈ 40K chars of render.
	a.largeModel = csync.NewValue(Model{CatwalkCfg: catwalk.Model{
		ContextWindow:    64_000,
		DefaultMaxTokens: 4_000,
	}})
	// A huge raw budget keeps the boundary at 0 so engagement shows as
	// in-place collapse, not eviction — the mechanism this test pins.
	a.rawTokenBudget = 1_000_000
	a.reqStats = csync.NewMap[string, requestStats]()
	msgs := priorTurnFixture(t, svc, sessionID)
	ctx := t.Context()

	collapse := a.newTurnCollapse(1)
	history, _ := a.preparePrompt(ctx, msgs, false, collapse, false)
	require.Nil(t, collapse.Set, "below pressure the set stays unfrozen")
	require.Contains(t, renderedResultText(t, history, "tc-bash"), "file content line")
	rs, _ := a.reqStats.Get(sessionID)
	require.False(t, rs.pressureEngaged)

	// A 200KB stored message lands between renders — appended history
	// crosses the margin before the next render evaluates.
	mkMsg(t, svc, sessionID, message.User,
		message.TextContent{Text: strings.Repeat("filler ", 30_000)})
	var err error
	msgs, err = svc.List(ctx, sessionID)
	require.NoError(t, err)

	history, _ = a.preparePrompt(ctx, msgs, false, collapse, false)
	require.True(t, collapse.Set[0], "the engaged render collapses the covered prior turn")
	res := renderedResultText(t, history, "tc-bash")
	require.Contains(t, res, `recall("result:tc-bash")`)
	rs, _ = a.reqStats.Get(sessionID)
	require.True(t, rs.pressureEngaged)
	require.Equal(t, 1, rs.pressureActivations)
}

// A skipPressureGate render is Summarize's one-shot path: machinery
// stays on but the gate is skipped entirely — no estimate, no
// watermark move, no latch — so /compact can't corrupt the session's
// pressure state. On a window smaller than the margin, any
// evaluation would latch, so staying unlatched proves the gate never
// ran.
func TestPreparePrompt_SummarizeRenderSkipsGateState(t *testing.T) {
	t.Parallel()

	a, svc, _, sessionID := newSegmentTestAgent(t, echoEntryGen{})
	a.priorTurns = priorTurnsStub
	a.pressureGate = true
	a.largeModel = csync.NewValue(Model{CatwalkCfg: catwalk.Model{
		ContextWindow:    40_000,
		DefaultMaxTokens: 8_000,
	}})
	a.reqStats = csync.NewMap[string, requestStats]()
	msgs := priorTurnFixture(t, svc, sessionID)
	a.reqStats.Set(sessionID, requestStats{LastPromptTokens: 5_000, renderedMsgs: 2, pressureEstimate: 5_500})

	history, _ := a.preparePrompt(t.Context(), msgs, false, nil, true)
	require.NotEmpty(t, history)

	rs, _ := a.reqStats.Get(sessionID)
	require.Equal(t, int64(5_500), rs.pressureEstimate, "one-shot renders must not move the estimate")
	require.Equal(t, 2, rs.renderedMsgs, "one-shot renders must not move the watermark")
	require.False(t, rs.pressureEngaged, "a /compact must not trip the latch")
	require.Zero(t, rs.pressureActivations)
}

// A stored summary is a hard render floor in notebook mode: /summarize
// must actually compact, not append a placebo message. The rendered
// tail starts at the summary — role-flipped to user like the
// non-notebook path — and pre-summary content never reaches the wire.
func TestPreparePrompt_SummaryFloorInNotebookMode(t *testing.T) {
	t.Parallel()

	a, svc, _, sessionID := newSegmentTestAgent(t, echoEntryGen{})
	a.reqStats = csync.NewMap[string, requestStats]()
	// A huge raw budget pins the coverage boundary at 0 so the test
	// isolates the summary floor, not the compaction machinery.
	a.rawTokenBudget = 1_000_000

	viewThenEdit(t, svc, sessionID, "file body", true)
	_, err := svc.Create(t.Context(), sessionID, message.CreateMessageParams{
		Role:             message.Assistant,
		Parts:            []message.ContentPart{message.TextContent{Text: "SUMMARY-BODY"}},
		IsSummaryMessage: true,
	})
	require.NoError(t, err)
	mkMsg(t, svc, sessionID, message.User, message.TextContent{Text: "after summary"})
	msgs, err := svc.List(t.Context(), sessionID)
	require.NoError(t, err)

	history, _ := a.preparePrompt(t.Context(), msgs, false, nil, false)
	text := renderedText(history)
	require.Contains(t, text, "SUMMARY-BODY")
	require.Contains(t, text, "after summary")
	require.NotContains(t, text, "start")
	require.NotContains(t, text, "edit it")
	require.NotContains(t, text, "next")

	// The summary head renders as user — strict-adjacency providers
	// reject an assistant-role first message.
	for _, m := range history {
		for _, p := range m.Content {
			if tp, ok := p.(fantasy.TextPart); ok && strings.Contains(tp.Text, "SUMMARY-BODY") {
				require.Equal(t, fantasy.MessageRoleUser, m.Role)
			}
		}
	}
}

// Verbatim mode is the default and passes a nil collapse — the gate
// must still evaluate: it covers boundary/prefix churn in every mode,
// and gate state must be written or pressure.* telemetry starves on
// the most common configuration.
func TestPreparePrompt_PressureGateVerbatimModeEvaluates(t *testing.T) {
	t.Parallel()

	a, svc, _, sessionID := newSegmentTestAgent(t, echoEntryGen{})
	a.priorTurns = priorTurnsVerbatim // the default — nil collapse.
	a.pressureGate = true
	a.largeModel = csync.NewValue(Model{CatwalkCfg: catwalk.Model{
		ContextWindow:    64_000,
		DefaultMaxTokens: 4_000,
	}})
	a.reqStats = csync.NewMap[string, requestStats]()
	msgs := priorTurnFixture(t, svc, sessionID)
	ctx := t.Context()

	// Below the margin: evaluated (estimate lands), no latch.
	_, _ = a.preparePrompt(ctx, msgs, false, nil, false)
	rs, ok := a.reqStats.Get(sessionID)
	require.True(t, ok, "verbatim renders must still record gate state")
	require.Positive(t, rs.pressureEstimate)
	require.False(t, rs.pressureEngaged)

	// Past the margin: the verbatim-mode render engages — boundary
	// and prefix machinery apply in every mode, not just collapse.
	mkMsg(t, svc, sessionID, message.User,
		message.TextContent{Text: strings.Repeat("filler ", 30_000)})
	var err error
	msgs, err = svc.List(ctx, sessionID)
	require.NoError(t, err)
	_, _ = a.preparePrompt(ctx, msgs, false, nil, false)
	rs, _ = a.reqStats.Get(sessionID)
	require.True(t, rs.pressureEngaged)
	require.Equal(t, 1, rs.pressureActivations)
}

// A successful compact shrinks stored history — the one write that
// breaks the append-only premise — so the latch and its anchors
// reset and the next render re-derives engagement.
func TestPressureGate_CompactClearsLatch(t *testing.T) {
	t.Parallel()

	a := pressureTestAgent(1_000_000, 8_000)
	a.reqStats.Set("s1", requestStats{
		LastPromptTokens:    950_000,
		renderedMsgs:        40,
		pressureEstimate:    960_000,
		pressureEngaged:     true,
		pressureActivations: 1,
	})

	a.clearPressureState("s1")
	rs, _ := a.reqStats.Get("s1")
	require.False(t, rs.pressureEngaged)
	require.Zero(t, rs.pressureEstimate)
	require.Zero(t, rs.renderedMsgs)
	require.Zero(t, rs.LastPromptTokens)
	require.Equal(t, 1, rs.pressureActivations, "activation history survives the reset")

	// The next render re-derives from the new, smaller history — a
	// compacted session is comfortable again.
	require.False(t, a.pressureEngaged("s1", []message.Message{segUser("small")}))
}

// The digest-eligibility freeze has the same empty-freeze edge as
// collapse.Set: a disengaged render passes a zero boundary key, under
// which no entry qualifies — freezing {} there would lock digests out
// for the run even after the gate engages.
func TestFreezeDigestEligibility_EmptyRenderDefers(t *testing.T) {
	t.Parallel()

	collapse := &turnCollapse{Before: 1}
	entries := []notebook.Entry{{
		EventType:     notebook.EventCheckpoint,
		TurnNumber:    0,
		SegmentNumber: 0,
		Tags:          []string{"granularity:turn"},
	}}

	// Zero boundary key — the disengaged path — sees nothing.
	freezeDigestEligibility(collapse, entries, segmentKey{})
	require.Nil(t, collapse.digestTurns)

	// A later engaged render (real boundary) freezes the digest in.
	freezeDigestEligibility(collapse, entries, segmentKey{turn: 1})
	require.Equal(t, map[int64]bool{0: true}, collapse.digestTurns)
}

func TestEnforceWindowCap(t *testing.T) {
	t.Parallel()

	big := []fantasy.Message{fantasy.NewUserMessage(strings.Repeat("x", 240_000))} // ~60K est.
	small := []fantasy.Message{fantasy.NewUserMessage("hi")}

	t.Run("flag off never rejects", func(t *testing.T) {
		a := pressureTestAgent(64_000, 4_000)
		require.NoError(t, a.enforceWindowCap(big, nil, 4_000))
	})
	t.Run("unknown window never rejects", func(t *testing.T) {
		a := pressureTestAgent(0, 4_000)
		a.enforceContextWindow = true
		require.NoError(t, a.enforceWindowCap(big, nil, 4_000))
	})
	t.Run("under cap passes", func(t *testing.T) {
		a := pressureTestAgent(64_000, 4_000)
		a.enforceContextWindow = true
		require.NoError(t, a.enforceWindowCap(small, nil, 4_000))
	})
	t.Run("over cap rejects with sentinel", func(t *testing.T) {
		a := pressureTestAgent(64_000, 4_000)
		a.enforceContextWindow = true
		err := a.enforceWindowCap(big, nil, 4_000)
		require.ErrorIs(t, err, ErrContextWindowExceeded)
		require.Contains(t, err.Error(), "declared window")
	})
	t.Run("output budget counts against the window", func(t *testing.T) {
		// ~60K input fits a 64K window with a 4K completion but not
		// with an 8K one — the provider contract is input + output.
		a := pressureTestAgent(64_000, 4_000)
		a.enforceContextWindow = true
		require.NoError(t, a.enforceWindowCap(big, nil, 3_000))
		require.ErrorIs(t, a.enforceWindowCap(big, nil, 8_000), ErrContextWindowExceeded)
	})
	t.Run("unset output budget falls back to catalog default", func(t *testing.T) {
		a := pressureTestAgent(64_000, 8_000)
		a.enforceContextWindow = true
		// ~60K input + 8K catalog default > 64K — the same failure
		// a real endpoint returns when max_tokens pushes over.
		require.ErrorIs(t, a.enforceWindowCap(big, nil, 0), ErrContextWindowExceeded)
	})
	t.Run("tool schemas count against the window", func(t *testing.T) {
		// ~60K input + 4K output fits 64K; a ~20K-token schema block
		// (providers bill it on every request) pushes the same
		// request over.
		a := pressureTestAgent(64_000, 4_000)
		a.enforceContextWindow = true
		tool := fantasy.NewAgentTool("big_tool", strings.Repeat("d", 80_000),
			func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
				return fantasy.ToolResponse{}, nil
			})
		require.NoError(t, a.enforceWindowCap(big, nil, 3_000))
		require.ErrorIs(t, a.enforceWindowCap(big, []fantasy.AgentTool{tool}, 3_000), ErrContextWindowExceeded)
	})
}

// TestRun_EnforceContextWindowCap pins the two-arm contract #112 needs:
// with a manifest-pinned 64K window and enforcement on, a verbatim
// (notebook-off) run dies exactly like a small-window endpoint would
// kill it — terminal error, nothing reaches the wire — while the same
// history under the gate collapses under the cap and completes.
func TestRun_EnforceContextWindowCap(t *testing.T) {
	t.Parallel()

	// A covered turn carrying a ~280KB tool result: ~70K estimated
	// tokens — over a 64K window verbatim, a stub line collapsed.
	seed := func(t *testing.T, env *processEnv, sessionID string) []message.Message {
		t.Helper()
		mkMsg(t, env.messages, sessionID, message.User, message.TextContent{Text: "first prompt"})
		mkMsg(t, env.messages, sessionID, message.Assistant,
			message.ToolCall{ID: "tc-big", Name: "bash", Input: `{"command":"cat huge.log"}`, Finished: true})
		mkMsg(t, env.messages, sessionID, message.Tool,
			message.ToolResult{ToolCallID: "tc-big", Name: "bash", Content: strings.Repeat("result ", 40_000)})
		mkMsg(t, env.messages, sessionID, message.Assistant, message.TextContent{Text: "turn zero answer"})
		mkMsg(t, env.messages, sessionID, message.User, message.TextContent{Text: "second prompt"})
		msgs, err := env.messages.List(t.Context(), sessionID)
		require.NoError(t, err)
		return msgs
	}
	mk := func(env *processEnv, m fantasy.LanguageModel, notebookOn bool) *sessionAgent {
		model := Model{Model: m, CatwalkCfg: catwalk.Model{ContextWindow: 64_000, DefaultMaxTokens: 4_000}}
		opts := SessionAgentOptions{
			LargeModel:           model,
			SmallModel:           model,
			SystemPrompt:         "system",
			IsYolo:               true,
			Sessions:             env.sessions,
			Messages:             env.messages,
			NotebookEnabled:      notebookOn,
			NotebookPriorTurns:   priorTurnsStub,
			PressureGate:         true,
			EnforceContextWindow: true,
			RawTokenBudget:       1_000_000,
			DetachedWork:         &sync.WaitGroup{},
			Tools: []fantasy.AgentTool{
				&fakeTool{name: "bash", resp: fantasy.NewTextResponse("ok")},
				&fakeTool{name: notebooktool.RecallToolName, resp: fantasy.NewTextResponse("recalled")},
				&fakeTool{name: notebooktool.SearchToolName, resp: fantasy.NewTextResponse("")},
			},
		}
		if notebookOn {
			opts.Notebook = env.notebook
		}
		return NewSessionAgent(opts).(*sessionAgent)
	}

	t.Run("control dies at the cap like a real endpoint", func(t *testing.T) {
		env, sessionID := newProcessEnv(t, &countingGen{})
		seed(t, env, sessionID)

		m := &captureModel{usage: 2_000}
		a := mk(env, m, false)

		_, err := a.Run(t.Context(), SessionAgentCall{SessionID: sessionID, Prompt: "turn two"})
		require.ErrorIs(t, err, ErrContextWindowExceeded)
		require.Empty(t, m.prompts, "the rejected request must never reach the model")
	})

	t.Run("treatment collapses under the cap and completes", func(t *testing.T) {
		env, sessionID := newProcessEnv(t, &countingGen{})
		msgs := seed(t, env, sessionID)

		m := &captureModel{usage: 2_000}
		a := mk(env, m, true)

		// Commit coverage directly — the run's own detectSegments sees
		// the seeded turn as already processed, so the cold-start
		// estimate engages the gate and the covered turn collapses.
		ctx := t.Context()
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
		require.NoError(t, env.notebook.MarkSegmentsProcessed(ctx, sessionID, covered))

		_, err := a.Run(ctx, SessionAgentCall{SessionID: sessionID, Prompt: "turn two"})
		require.NoError(t, err)
		a.detachedWork.Wait()

		require.NotEmpty(t, m.prompts, "the collapsed render must reach the wire")
		wire := m.prompts[len(m.prompts)-1]
		require.Contains(t, wire, `{"_collapsed":"prior turn 0"}`)
		require.NotContains(t, wire, "result result result",
			"the giant result must not reach the wire — it is what would have overflowed")
	})
}
