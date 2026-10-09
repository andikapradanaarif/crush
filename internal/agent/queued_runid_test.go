package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// gatedStreamModel streams a single text part followed by a clean finish,
// but blocks the very first Stream call until its gate is released. That
// lets a test hold a run "active" (past PrepareStep, inside Stream) just
// long enough to enqueue a follow-up prompt behind the busy session.
// Subsequent Stream calls (e.g. the loop's queued turns draining the
// queue) proceed immediately.
type gatedStreamModel struct {
	text    string
	gate    chan struct{}
	entered chan struct{}
	calls   atomic.Int64
}

func (m *gatedStreamModel) Provider() string { return "fake" }
func (m *gatedStreamModel) Model() string    { return "fake-model" }

func (m *gatedStreamModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: m.text}},
		FinishReason: fantasy.FinishReasonStop,
	}, nil
}

func (m *gatedStreamModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if m.calls.Add(1) == 1 {
		close(m.entered)
		select {
		case <-m.gate:
		case <-ctx.Done():
		}
	}
	text := m.text
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
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *gatedStreamModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *gatedStreamModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// stackDepthModel wraps gatedStreamModel and captures the goroutine
// stack on every Stream call after the gated first one, so a test can
// count the sessionAgent.Run frames a queued turn executes under. One
// frame proves the iterative handoff; the old recursion nested one Run
// frame per queued prompt.
type stackDepthModel struct {
	gatedStreamModel
	mu     sync.Mutex
	stacks [][]byte
}

func (m *stackDepthModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if m.calls.Load() > 0 {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, false)
		m.mu.Lock()
		m.stacks = append(m.stacks, buf[:n])
		m.mu.Unlock()
	}
	return m.gatedStreamModel.Stream(ctx, call)
}

// TestRun_QueuedRunIDPromptRunsAsOwnTurnAndPublishesRunComplete is the
// end-to-end proof of fix 2: a prompt carrying a RunID that is queued
// behind a busy session must NOT be silently folded into the active turn.
// It runs as its own turn via the queued-handoff path and publishes its
// own terminal RunComplete, so a `crush run` caller blocking on that
// RunID does not hang. The active turn keeps its own RunComplete too.
func TestRun_QueuedRunIDPromptRunsAsOwnTurnAndPublishesRunComplete(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	broker := pubsub.NewBroker[notify.RunComplete]()
	t.Cleanup(broker.Shutdown)

	large := &gatedStreamModel{
		text:    "done",
		gate:    make(chan struct{}),
		entered: make(chan struct{}),
	}
	small := &finishStreamModel{text: "title"}

	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel:  Model{Model: large, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		SmallModel:  Model{Model: small, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		IsYolo:      true,
		Sessions:    env.sessions,
		Messages:    env.messages,
		RunComplete: broker,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	subCtx, subCancel := context.WithCancel(t.Context())
	defer subCancel()
	ch := broker.Subscribe(subCtx)

	// Start the main turn; it blocks inside Stream once active.
	mainDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{
			SessionID: sess.ID,
			RunID:     "run-main",
			Prompt:    "main",
		})
		mainDone <- runErr
	}()

	// Wait until the main turn is active (inside Stream).
	select {
	case <-large.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("main run never entered Stream")
	}
	require.True(t, sa.IsSessionBusy(sess.ID), "main run must be active before enqueueing the follow-up")

	// Enqueue a RunID-bearing follow-up behind the busy session.
	res, err := sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		RunID:     "run-follow",
		Prompt:    "follow",
	})
	require.NoError(t, err)
	require.Nil(t, res, "a busy-session follow-up must enqueue and return (nil, nil)")
	require.Equal(t, 1, sa.QueuedPrompts(sess.ID), "the follow-up must be queued, not folded")

	// Release the main turn so it completes and hands off to the queue.
	close(large.gate)
	require.NoError(t, <-mainDone)

	// Both turns must publish their own terminal RunComplete.
	got := map[string]notify.RunComplete{}
	deadline := time.After(5 * time.Second)
	for len(got) < 2 {
		select {
		case ev := <-ch:
			got[ev.Payload.RunID] = ev.Payload
		case <-deadline:
			t.Fatalf("timed out waiting for both RunCompletes; got %v", got)
		}
	}

	main, ok := got["run-main"]
	require.True(t, ok, "the active turn must publish its own RunComplete")
	require.Empty(t, main.Error)
	require.False(t, main.Cancelled)

	follow, ok := got["run-follow"]
	require.True(t, ok,
		"the queued RunID prompt must publish its own RunComplete instead of being folded silently")
	require.Empty(t, follow.Error)
	require.False(t, follow.Cancelled)
	require.Equal(t, "done", follow.Text, "the queued prompt ran as its own turn")

	// Two distinct assistant turns prove the follow-up was not folded.
	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var assistants, follows int
	for _, m := range msgs {
		switch m.Role {
		case message.Assistant:
			assistants++
		case message.User:
			if m.Content().String() == "follow" {
				follows++
			}
		}
	}
	require.Equal(t, 2, assistants, "the active turn and the queued turn each produce one assistant message")
	require.Equal(t, 1, follows, "the follow-up prompt is its own user turn")
}

// TestRun_QueuedPromptsRunInOneRunFrame is the regression guard for the
// recursion-to-loop conversion: each queued prompt must execute its turn
// inside the single Run loop frame, not under a nested Run call. Two
// queued prompts are enough — under the old recursion the second queued
// turn would sit three Run frames deep.
func TestRun_QueuedPromptsRunInOneRunFrame(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	broker := pubsub.NewBroker[notify.RunComplete]()
	t.Cleanup(broker.Shutdown)

	large := &stackDepthModel{gatedStreamModel: gatedStreamModel{
		text:    "done",
		gate:    make(chan struct{}),
		entered: make(chan struct{}),
	}}
	small := &finishStreamModel{text: "title"}

	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel:  Model{Model: large, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		SmallModel:  Model{Model: small, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		IsYolo:      true,
		Sessions:    env.sessions,
		Messages:    env.messages,
		RunComplete: broker,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	subCtx, subCancel := context.WithCancel(t.Context())
	defer subCancel()
	ch := broker.Subscribe(subCtx)

	mainDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{
			SessionID: sess.ID,
			RunID:     "run-main",
			Prompt:    "main",
		})
		mainDone <- runErr
	}()

	select {
	case <-large.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("main run never entered Stream")
	}

	// Enqueue two follow-ups behind the busy session.
	for i, runID := range []string{"run-q1", "run-q2"} {
		res, err := sa.Run(t.Context(), SessionAgentCall{
			SessionID: sess.ID,
			RunID:     runID,
			Prompt:    fmt.Sprintf("follow-%d", i),
		})
		require.NoError(t, err)
		require.Nil(t, res)
	}
	require.Equal(t, 2, sa.QueuedPrompts(sess.ID))

	close(large.gate)
	require.NoError(t, <-mainDone)

	// Drain all three RunCompletes so every queued turn has finished.
	got := map[string]notify.RunComplete{}
	deadline := time.After(5 * time.Second)
	for len(got) < 3 {
		select {
		case ev := <-ch:
			got[ev.Payload.RunID] = ev.Payload
		case <-deadline:
			t.Fatalf("timed out waiting for RunCompletes; got %v", got)
		}
	}

	large.mu.Lock()
	defer large.mu.Unlock()
	require.Len(t, large.stacks, 2, "each queued prompt should run one turn")
	for i, stack := range large.stacks {
		frames := bytes.Count(stack, []byte("internal/agent.(*sessionAgent).Run("))
		require.Equal(t, 1, frames,
			"queued turn %d ran nested Run frames — the handoff must iterate, not recurse\n%s", i, stack)
	}
}
