package agent

import (
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
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

	// The step-jump reserve dominates whenever the flat legacy margin
	// is smaller — a 64K window still carries output reserve + four
	// capped tool results.
	require.Equal(t, int64(8_000+4*12_500), pressureMargin(1_000_000, 8_000))
	require.Equal(t, int64(8_000+4*12_500), pressureMargin(64_000, 8_000))
	// A large catalog output reserve raises the margin further — the
	// request must leave real generation headroom.
	require.Equal(t, int64(384_000+4*12_500), pressureMargin(1_000_000, 384_000))
	// Below the step jump, the legacy 20% floor never wins on small
	// windows; on very large ones the flat 20K still loses to the jump.
	require.Equal(t, int64(4*12_500+4_096), pressureMargin(150_000, 4_096))
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

	// No declared window means no evidence of headroom — the render
	// machinery stays on (pre-gate behavior), but nothing latches or
	// counts: "engaged" here is the default, not an activation.
	require.True(t, a.pressureEngaged("s1", msgs))
	_, ok := a.reqStats.Get("s1")
	require.False(t, ok, "unknown window must not write gate state")
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
	history, _ := a.preparePrompt(t.Context(), msgs, false, collapse)

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
	history, _ := a.preparePrompt(t.Context(), msgs, false, collapse)

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
	history, _ := a.preparePrompt(t.Context(), msgs, false, collapse)

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
	history, _ := a.preparePrompt(ctx, unclosed, false, collapse)
	require.Nil(t, collapse.Set, "empty coverage must not freeze the set")

	// The closing user message lands. This render fires turn 0's
	// coverage but reads the registry pre-commit — still uncovered,
	// still unfrozen.
	history, _ = a.preparePrompt(ctx, msgs, false, collapse)
	require.Nil(t, collapse.Set)

	// The next render observes the committed coverage — the same
	// run's collapse set activates late rather than never.
	history, _ = a.preparePrompt(ctx, msgs, false, collapse)
	require.NotNil(t, collapse.Set)
	require.True(t, collapse.Set[0])
	res := renderedResultText(t, history, "tc-bash")
	require.Contains(t, res, `recall("result:tc-bash")`)
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
