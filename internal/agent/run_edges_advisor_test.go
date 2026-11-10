package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/catwalk/v3/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// advisorSpyModel is a fantasy.LanguageModel that streams a fixed
// reply while recording call count and the joined prompt text — the
// assertion surface for what evidence the advisor saw.
type advisorSpyModel struct {
	text      string
	err       error
	finish    fantasy.FinishReason
	calls     int
	prompt    string
	maxTokens int64
}

func (m *advisorSpyModel) Provider() string { return "fake" }
func (m *advisorSpyModel) Model() string    { return "advisor-fake" }

func (m *advisorSpyModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *advisorSpyModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls++
	var sb strings.Builder
	for _, msg := range call.Prompt {
		for _, part := range msg.Content {
			if tp, ok := part.(fantasy.TextPart); ok {
				sb.WriteString(tp.Text)
			}
		}
	}
	m.prompt = sb.String()
	if call.MaxOutputTokens != nil {
		m.maxTokens = *call.MaxOutputTokens
	}
	if m.err != nil {
		return nil, m.err
	}
	text := m.text
	finish := m.finish
	if finish == "" {
		finish = fantasy.FinishReasonStop
	}
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: text}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "1"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: finish})
	}, nil
}

func (m *advisorSpyModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *advisorSpyModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// spyAdvisor wires an advisorSpyModel into a test agent's resolver —
// the advisor runs whenever an edge fires.
func spyAdvisor(m *advisorSpyModel) func(context.Context, string) Model {
	return func(context.Context, string) Model {
		return Model{Model: m}
	}
}

func TestEdgeAdvisorInjectsAdvice(t *testing.T) {
	t.Parallel()

	a, svc, sessionID := newGateTestAgent(t, &config.Config{})
	a.ambiguityClarification = true
	a.interactive = true
	a.tools = csync.NewSliceFrom([]fantasy.AgentTool{
		&fakeTool{name: tools.TodosToolName},
		&fakeTool{name: tools.QuestionToolName},
	})
	spy := &advisorSpyModel{text: "test a different hypothesis — the lexer's EOF path, not the test."}
	a.advisorModel = spyAdvisor(spy)
	_, err := svc.Create(t.Context(), sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "unblock the release build"}},
	})
	require.NoError(t, err)

	queued := runEdgesForTest(a, t.Context(),
		SessionAgentCall{SessionID: sessionID, RepairAttempts: 1},
		edgeInput{result: stallResult(), stalled: true})
	require.True(t, queued)
	require.Equal(t, 1, spy.calls)

	// The advisor saw the deterministic evidence — edge identity plus
	// the run-state handoff — not the escalation instruction.
	require.Contains(t, spy.prompt, "Edge: stall")
	require.Contains(t, spy.prompt, "- Goal: unblock the release build")
	require.Contains(t, spy.prompt, "deterministic tripwire")

	q, _ := a.messageQueue.Get(sessionID)
	require.Len(t, q, 1)
	require.Contains(t, q[0].Prompt,
		"Advisor note — a reviewer model read the run state above and recommends: test a different hypothesis")
	// The repair prompt's fingerprints both survive — the edge prefix
	// still leads and the outro still closes.
	require.True(t, strings.HasPrefix(q[0].Prompt, stallRetryPrefix))
	require.True(t, IsRepairPrompt(q[0].Prompt))
}

func TestEdgeAdvisorSilentWithoutFire(t *testing.T) {
	t.Parallel()

	a, _, sessionID := newGateTestAgent(t, &config.Config{})
	a.ambiguityClarification = true
	spy := &advisorSpyModel{text: "irrelevant"}
	a.advisorModel = spyAdvisor(spy)

	queued := runEdgesForTest(a, t.Context(),
		SessionAgentCall{SessionID: sessionID},
		edgeInput{result: cleanResult()})
	require.False(t, queued)
	// The standing rule: no trigger, no advisor call.
	require.Equal(t, 0, spy.calls)
}

func TestEdgeAdvisorFailOpen(t *testing.T) {
	t.Parallel()

	a, _, sessionID := newGateTestAgent(t, &config.Config{})
	a.ambiguityClarification = true
	a.interactive = true
	a.tools = csync.NewSliceFrom([]fantasy.AgentTool{
		&fakeTool{name: tools.TodosToolName},
		&fakeTool{name: tools.QuestionToolName},
	})
	spy := &advisorSpyModel{err: errors.New("provider down")}
	a.advisorModel = spyAdvisor(spy)

	queued := runEdgesForTest(a, t.Context(),
		SessionAgentCall{SessionID: sessionID, RepairAttempts: 1},
		edgeInput{result: stallResult(), stalled: true})
	// A failed advisor call changes nothing — the deterministic
	// repair proceeds unannotated.
	require.True(t, queued)
	require.Equal(t, 1, spy.calls)
	q, _ := a.messageQueue.Get(sessionID)
	require.Len(t, q, 1)
	require.NotContains(t, q[0].Prompt, "Advisor note")
	require.True(t, strings.HasPrefix(q[0].Prompt, stallRetryPrefix))
}

func TestEdgeAdvisorReasoningBudget(t *testing.T) {
	t.Parallel()

	build := func() (*sessionAgent, *advisorSpyModel, string, message.Service) {
		a, svc, sessionID := newGateTestAgent(t, &config.Config{})
		a.ambiguityClarification = true
		a.interactive = true
		a.tools = csync.NewSliceFrom([]fantasy.AgentTool{
			&fakeTool{name: tools.TodosToolName},
			&fakeTool{name: tools.QuestionToolName},
		})
		spy := &advisorSpyModel{text: "try a different hypothesis."}
		return a, spy, sessionID, svc
	}

	// A reasoning-capable advisor model gets the catwalk default
	// budget — a small cap is spent on reasoning_content and the
	// reply comes back empty (the scope resolver's documented
	// truncation).
	a, spy, sessionID, svc := build()
	a.advisorModel = func(context.Context, string) Model {
		return Model{Model: spy, CatwalkCfg: catwalk.Model{
			DefaultMaxTokens: 4096,
			Reasoning:        catwalk.Reasoning{Thinking: catwalk.ThinkingAlways},
		}}
	}
	_, err := svc.Create(t.Context(), sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "goal"}},
	})
	require.NoError(t, err)
	require.True(t, runEdgesForTest(a, t.Context(),
		SessionAgentCall{SessionID: sessionID, RepairAttempts: 1},
		edgeInput{result: stallResult(), stalled: true}))
	require.Equal(t, int64(4096), spy.maxTokens)

	// A plain advisor model gets the 1024 floor — the same bound the
	// scope resolver sets as its minimum.
	a, spy, sessionID, _ = build()
	a.advisorModel = func(context.Context, string) Model { return Model{Model: spy} }
	require.True(t, runEdgesForTest(a, t.Context(),
		SessionAgentCall{SessionID: sessionID, RepairAttempts: 1},
		edgeInput{result: stallResult(), stalled: true}))
	require.Equal(t, int64(advisorMinTokens), spy.maxTokens)
}

func TestEdgeAdvisorLengthFinishSkips(t *testing.T) {
	t.Parallel()

	a, _, sessionID := newGateTestAgent(t, &config.Config{})
	a.ambiguityClarification = true
	a.interactive = true
	a.tools = csync.NewSliceFrom([]fantasy.AgentTool{
		&fakeTool{name: tools.TodosToolName},
		&fakeTool{name: tools.QuestionToolName},
	})
	// A length-truncated reply is no advice — annotating a
	// mid-sentence fragment is worse than the bare prompt.
	spy := &advisorSpyModel{text: "first try the", finish: fantasy.FinishReasonLength}
	a.advisorModel = spyAdvisor(spy)

	queued := runEdgesForTest(a, t.Context(),
		SessionAgentCall{SessionID: sessionID, RepairAttempts: 1},
		edgeInput{result: stallResult(), stalled: true})
	require.True(t, queued)
	q, _ := a.messageQueue.Get(sessionID)
	require.Len(t, q, 1)
	require.NotContains(t, q[0].Prompt, "Advisor note")
}

func TestAdvisorEvidenceDedupesFallbackCheck(t *testing.T) {
	t.Parallel()

	a, _, sessionID := newGateTestAgent(t, &config.Config{})
	failed := []gateCheckOutcome{{
		check: message.VerificationCheck{
			Check:  "diagnostics",
			State:  "failed",
			Detail: "2 new error(s)",
		},
	}}
	trigger := &edgeTrigger{failed: failed, checkFallback: &failed[0]}
	evidence := a.advisorEvidence(t.Context(), SessionAgentCall{SessionID: sessionID},
		[]contender{{edge: runEdge{name: "verification"}, t: trigger}})

	// The handoff's verification line already names the latest
	// verdict — the check list must not print it a second time.
	require.Contains(t, evidence, "- Verification: check `diagnostics` failed")
	require.Equal(t, 1, strings.Count(evidence, "diagnostics"))
}

func TestEdgeAdvisorNilResolverSkips(t *testing.T) {
	t.Parallel()

	a, _, sessionID := newGateTestAgent(t, &config.Config{})
	a.ambiguityClarification = true
	a.interactive = true
	a.tools = csync.NewSliceFrom([]fantasy.AgentTool{
		&fakeTool{name: tools.TodosToolName},
		&fakeTool{name: tools.QuestionToolName},
	})
	// advisorModel nil — the options.edge_advisor off state.
	queued := runEdgesForTest(a, t.Context(),
		SessionAgentCall{SessionID: sessionID, RepairAttempts: 1},
		edgeInput{result: stallResult(), stalled: true})
	require.True(t, queued)
	q, _ := a.messageQueue.Get(sessionID)
	require.Len(t, q, 1)
	require.NotContains(t, q[0].Prompt, "Advisor note")
}

func TestEdgeAdvisorEnabledResolution(t *testing.T) {
	t.Parallel()

	var nilOpts *config.Options
	require.False(t, nilOpts.EdgeAdvisorEnabled())
	require.False(t, (&config.Options{}).EdgeAdvisorEnabled())
	off := false
	require.False(t, (&config.Options{EdgeAdvisor: &off}).EdgeAdvisorEnabled())
	on := true
	require.True(t, (&config.Options{EdgeAdvisor: &on}).EdgeAdvisorEnabled())
}

func TestAdvisorModelFnResolution(t *testing.T) {
	t.Parallel()

	large := Model{Model: &advisorSpyModel{}}
	sessionModels := func(string) ModelSet { return ModelSet{Large: large} }
	c := &coordinator{cfg: config.NewTestStore(&config.Config{})}

	// Off: nil Options and EdgeAdvisor=false both return nil.
	require.Nil(t, c.advisorModelFn(sessionModels, nil))
	off := false
	require.Nil(t, c.advisorModelFn(sessionModels, &config.Options{EdgeAdvisor: &off}))

	on := true
	// Enabled with no key: the session's large slot serves.
	fn := c.advisorModelFn(sessionModels, &config.Options{EdgeAdvisor: &on})
	require.NotNil(t, fn)
	require.Equal(t, large.Model, fn(context.Background(), "s").Model)

	// Enabled with an unknown key: warns and still serves large.
	fn = c.advisorModelFn(sessionModels,
		&config.Options{EdgeAdvisor: &on, EdgeAdvisorModel: "no-such-key"})
	require.NotNil(t, fn)
	require.Equal(t, large.Model, fn(context.Background(), "s").Model)
}

func TestAdvisorModelFnNamedKeyBuilds(t *testing.T) {
	t.Parallel()

	c, _ := newOverrideTestCoordinator(t)
	c.cfg.Config().Models["advisor-x"] = config.SelectedModel{Provider: "mock", Model: "mock-model"}

	large := Model{Model: &advisorSpyModel{}}
	sessionModels := func(string) ModelSet { return ModelSet{Large: large} }

	on := true
	fn := c.advisorModelFn(sessionModels,
		&config.Options{EdgeAdvisor: &on, EdgeAdvisorModel: "advisor-x"})
	require.NotNil(t, fn)

	// The named key builds its own model — the session's large slot
	// is not consulted, whatever sessionID arrives.
	m := fn(t.Context(), "s")
	require.NotNil(t, m.Model)
	require.Equal(t, "mock-model", m.ModelCfg.Model)
	require.Equal(t, "mock-model", fn(t.Context(), "other").ModelCfg.Model)
}

func TestAdvisorModelFnNamedKeyUnknownFallsBack(t *testing.T) {
	t.Parallel()

	c, _ := newOverrideTestCoordinator(t)
	large := Model{Model: &advisorSpyModel{}}
	sessionModels := func(string) ModelSet { return ModelSet{Large: large} }

	on := true
	fn := c.advisorModelFn(sessionModels,
		&config.Options{EdgeAdvisor: &on, EdgeAdvisorModel: "not-a-model"})
	require.NotNil(t, fn)
	require.Equal(t, large.Model, fn(t.Context(), "s").Model)
}
