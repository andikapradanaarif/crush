package agent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	notebooktool "github.com/charmbracelet/crush/internal/agent/tools/notebook"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/notebook"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// toolThenTextModel answers its first main-turn Stream call with a bash
// tool call and every later call with a text finish — enough to give the
// run's turn a collapsible event without a provider. Title generation
// shares the model and must not consume the tool-call call.
type toolThenTextModel struct {
	fakeLanguageModel
	calls atomic.Int64
}

func (m *toolThenTextModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if strings.Contains(promptText(call.Prompt), "Generate a concise title") {
		return gateTextStream("title"), nil
	}
	if m.calls.Add(1) != 1 {
		return gateTextStream("done"), nil
	}
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
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
	}, nil
}

// gateGen blocks Generate until released — it stands in for the seconds
// of LLM latency that made run-end coverage lose the race against
// process exit before the drain existed.
type gateGen struct {
	inner   *countingGen
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func newGateGen() *gateGen {
	return &gateGen{
		inner:   &countingGen{},
		release: make(chan struct{}),
		entered: make(chan struct{}),
	}
}

func (g *gateGen) Generate(ctx context.Context, sessionID string, events []notebook.EntryInput) ([]notebook.GeneratedEntry, error) {
	g.once.Do(func() { close(g.entered) })
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.inner.Generate(ctx, sessionID, events)
}

func (g *gateGen) GenerateCheckpoint(ctx context.Context, sessionID, input string) (notebook.GeneratedEntry, error) {
	return g.inner.GenerateCheckpoint(ctx, sessionID, input)
}

func (g *gateGen) GenerateDigest(ctx context.Context, sessionID, input string) (notebook.GeneratedEntry, error) {
	return g.inner.GenerateDigest(ctx, sessionID, input)
}

// processEnv holds the durable services a `crush run` subprocess shares:
// one SQLite store behind sessions, messages, and the notebook. Each
// "process" is a fresh sessionAgent over the same services — the
// in-process state (segment trackers, stub stats, in-flight marks) is
// what a real process boundary discards.
type processEnv struct {
	sessions session.Service
	messages message.Service
	notebook notebook.Service
}

func newProcessEnv(t *testing.T, gen notebook.Generator) (*processEnv, string) {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	sess, err := sessions.Create(t.Context(), "two-process")
	require.NoError(t, err)

	return &processEnv{
		sessions: sessions,
		messages: message.NewService(q),
		notebook: notebook.NewService(q, gen, notebook.Options{DB: conn, MaxEntryTokens: 10000, MaxNotebookTokens: 100000}),
	}, sess.ID
}

// agent builds one "process": a fresh sessionAgent with its own
// detached-work group over the shared services. model serves both model
// slots; priorTurns is the resolved notebook_prior_turns mode.
func (e *processEnv) agent(model fantasy.LanguageModel, priorTurns string) *sessionAgent {
	m := Model{Model: model, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}}
	return NewSessionAgent(SessionAgentOptions{
		LargeModel:         m,
		SmallModel:         m,
		SystemPrompt:       "system",
		IsYolo:             true,
		Sessions:           e.sessions,
		Messages:           e.messages,
		Notebook:           e.notebook,
		NotebookEnabled:    true,
		NotebookPriorTurns: priorTurns,
		DetachedWork:       &sync.WaitGroup{},
		// Collapse and stub rendering gate on the live tool set —
		// carry the notebook tools like a default coder agent.
		Tools: []fantasy.AgentTool{
			&fakeTool{name: "bash", resp: fantasy.NewTextResponse("ok")},
			&fakeTool{name: notebooktool.RecallToolName, resp: fantasy.NewTextResponse("recalled")},
			&fakeTool{name: notebooktool.SearchToolName, resp: fantasy.NewTextResponse("")},
		},
	}).(*sessionAgent)
}

// TestRun_DrainCommitsCoverageForNextProcess is the issue-82
// regression test: under `crush run` (one process per turn) the
// run-end coverage pass must commit before exit, or the next process's
// frozen collapse set sees no covered prior turn and turns_collapsed
// stays 0 — the starvation the eval gate reported.
func TestRun_DrainCommitsCoverageForNextProcess(t *testing.T) {
	t.Parallel()
	env, sessionID := newProcessEnv(t, &countingGen{})

	// Process 1: a turn with one finished tool call, then drain —
	// RunNonInteractive's drainDetachedWork equivalent.
	a1 := env.agent(&toolThenTextModel{}, priorTurnsStub)
	_, err := a1.Run(t.Context(), SessionAgentCall{SessionID: sessionID, Prompt: "turn one"})
	require.NoError(t, err)
	a1.detachedWork.Wait()

	// The turn's segment must be processed before the process exits —
	// pre-drain this row died with the goroutine.
	rows, err := env.notebook.ProcessedSegments(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotEmpty(t, rows, "run-end pass must record coverage")
	for _, r := range rows {
		require.Equal(t, notebook.SegmentProcessed, r.State)
	}

	// Process 2: a fresh agent — all in-process state discarded — on
	// the same session. Its first render must collapse turn 0.
	a2 := env.agent(&finishStreamModel{text: "done"}, priorTurnsStub)
	_, err = a2.Run(t.Context(), SessionAgentCall{SessionID: sessionID, Prompt: "turn two"})
	require.NoError(t, err)
	a2.detachedWork.Wait()

	stats, ok := a2.stubStats.Get(sessionID)
	require.True(t, ok, "turn 0 must collapse in the second process")
	require.Equal(t, 1, stats.TurnsCollapsed)
	require.Equal(t, 1, stats.EventsCollapsed)
}

// TestRun_UndrainedExitLosesRunEndCoverage pins the pre-drain failure
// mode: a generation in flight when the process exits leaves the
// segment unprocessed, so the coverage predicate starves. The drain
// join is what makes the commit deterministic.
func TestRun_UndrainedExitLosesRunEndCoverage(t *testing.T) {
	t.Parallel()
	gen := newGateGen()
	env, sessionID := newProcessEnv(t, gen)

	a1 := env.agent(&toolThenTextModel{}, priorTurnsStub)
	_, err := a1.Run(t.Context(), SessionAgentCall{SessionID: sessionID, Prompt: "turn one"})
	require.NoError(t, err)

	// The run-end generation is parked mid-flight. A process that
	// exits here — the pre-drain behavior — loses the coverage.
	select {
	case <-gen.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("run-end generation never started")
	}
	rows, err := env.notebook.ProcessedSegments(t.Context(), sessionID)
	require.NoError(t, err)
	for _, r := range rows {
		require.NotEqual(t, notebook.SegmentProcessed, r.State,
			"segment must stay unprocessed while generation is in flight")
	}

	// Releasing the generator plus the drain join commits the coverage.
	close(gen.release)
	a1.detachedWork.Wait()
	rows, err = env.notebook.ProcessedSegments(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	for _, r := range rows {
		require.Equal(t, notebook.SegmentProcessed, r.State)
	}
}

// TestRun_DrainWritesTurnDigest covers the digest arm of the same
// defect: generateTurnDigests exists only in the run-end goroutine, so
// digests.written could never be >0 for a short-lived process without
// the drain.
func TestRun_DrainWritesTurnDigest(t *testing.T) {
	t.Parallel()
	gen := &countingGen{}
	env, sessionID := newProcessEnv(t, gen)

	a1 := env.agent(&toolThenTextModel{}, priorTurnsDigest)
	_, err := a1.Run(t.Context(), SessionAgentCall{SessionID: sessionID, Prompt: "turn one"})
	require.NoError(t, err)
	a1.detachedWork.Wait()

	stats, ok := a1.nbStats.Get(sessionID)
	require.True(t, ok, "digest stats must exist for the session")
	require.GreaterOrEqual(t, stats.DigestsWritten, 1,
		"the finished turn's digest must commit before process exit")
}
