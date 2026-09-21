package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

// approveEnv builds a coordinator with just the services ApprovePlan
// touches: messages to read the plan, sessions to seed, and the gate to
// resolve.
func approveEnv(t *testing.T) (*coordinator, fakeEnv, session.Session) {
	t.Helper()
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	coord := &coordinator{
		cfg:       cfg,
		messages:  env.messages,
		sessions:  env.sessions,
		scopeGate: newScopeGate(nil, false, env.sessions),
	}
	return coord, env, sess
}

func writePlanMessage(t *testing.T, env fakeEnv, sessionID, text string) {
	t.Helper()
	_, err := env.messages.Create(t.Context(), sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: text}},
	})
	require.NoError(t, err)
}

func TestApprovePlan(t *testing.T) {
	t.Parallel()

	planWithItems := session.PlanStartMarker + "\n\n# Plan\n\n```crush-plan-items\n" +
		`[{"key":"seed","content":"Seed the items","evidence_paths":["internal/session/planseed.go"]}]` +
		"\n```\n\n" + session.PlanReadyMarker

	t.Run("seeds items and resolves the next run's gate", func(t *testing.T) {
		t.Parallel()
		coord, env, sess := approveEnv(t)
		writePlanMessage(t, env, sess.ID, planWithItems)

		require.NoError(t, coord.ApprovePlan(t.Context(), sess.ID))

		stored, err := env.sessions.Get(t.Context(), sess.ID)
		require.NoError(t, err)
		require.Len(t, stored.Todos, 1)
		require.Equal(t, "seed", stored.Todos[0].Key)
		require.Equal(t, session.PlanItemPending, stored.Todos[0].Status)
		require.Equal(t, []string{"internal/session/planseed.go"}, stored.Todos[0].EvidencePaths)

		// The approval is pending, not yet consumed — it lands on the
		// next run's stamp.
		coord.scopeGate.mu.Lock()
		require.True(t, coord.scopeGate.approved[sess.ID])
		coord.scopeGate.mu.Unlock()
	})

	t.Run("re-approval merges by minted ID and preserves status", func(t *testing.T) {
		t.Parallel()
		coord, env, sess := approveEnv(t)
		writePlanMessage(t, env, sess.ID, planWithItems)
		require.NoError(t, coord.ApprovePlan(t.Context(), sess.ID))

		stored, err := env.sessions.Get(t.Context(), sess.ID)
		require.NoError(t, err)
		stored.Todos[0].Status = session.PlanItemCompleted
		_, err = env.sessions.Save(t.Context(), stored)
		require.NoError(t, err)

		// A revised plan rewords the item under the same key and adds a
		// second item.
		revised := session.PlanStartMarker + "\n\n# Plan v2\n\n```crush-plan-items\n" +
			`[{"key":"seed","content":"Seed the items, reworded","evidence_paths":["internal/session/planseed.go"]},` +
			`{"key":"extra","content":"New work","depends_on":["seed"],"evidence_paths":["internal/agent"]}]` +
			"\n```\n\n" + session.PlanReadyMarker
		writePlanMessage(t, env, sess.ID, revised)
		require.NoError(t, coord.ApprovePlan(t.Context(), sess.ID))

		stored, err = env.sessions.Get(t.Context(), sess.ID)
		require.NoError(t, err)
		require.Len(t, stored.Todos, 2)
		require.Equal(t, session.PlanItemCompleted, stored.Todos[0].Status,
			"the surviving keyed item keeps its completed status")
		require.Equal(t, "Seed the items, reworded", stored.Todos[0].Content)
		require.Equal(t, []string{stored.Todos[0].ID}, stored.Todos[1].DependsOn)
	})

	t.Run("ready plan without an items block still resolves the gate", func(t *testing.T) {
		t.Parallel()
		coord, env, sess := approveEnv(t)
		writePlanMessage(t, env, sess.ID,
			session.PlanStartMarker+"\n\n# Plan\n\n"+session.PlanReadyMarker)

		require.NoError(t, coord.ApprovePlan(t.Context(), sess.ID))
		stored, err := env.sessions.Get(t.Context(), sess.ID)
		require.NoError(t, err)
		require.Empty(t, stored.Todos)
		coord.scopeGate.mu.Lock()
		require.True(t, coord.scopeGate.approved[sess.ID])
		coord.scopeGate.mu.Unlock()
	})

	t.Run("no ready plan is a conflict", func(t *testing.T) {
		t.Parallel()
		coord, env, sess := approveEnv(t)
		writePlanMessage(t, env, sess.ID, "just a regular reply")
		require.ErrorIs(t, coord.ApprovePlan(t.Context(), sess.ID), ErrNoReadyPlan)
	})

	t.Run("an empty items block clears previously seeded items", func(t *testing.T) {
		t.Parallel()
		coord, env, sess := approveEnv(t)
		writePlanMessage(t, env, sess.ID, planWithItems)
		require.NoError(t, coord.ApprovePlan(t.Context(), sess.ID))

		emptied := session.PlanStartMarker + "\n\n# Plan v2\n\n```crush-plan-items\n[]\n```\n\n" + session.PlanReadyMarker
		writePlanMessage(t, env, sess.ID, emptied)
		require.NoError(t, coord.ApprovePlan(t.Context(), sess.ID))

		stored, err := env.sessions.Get(t.Context(), sess.ID)
		require.NoError(t, err)
		require.Empty(t, stored.Todos, "a revised plan emitting no items must not leave stale seeds")
	})

	t.Run("items without evidence bindings are dropped, not seeded", func(t *testing.T) {
		t.Parallel()
		coord, env, sess := approveEnv(t)
		writePlanMessage(t, env, sess.ID,
			session.PlanStartMarker+"\n```crush-plan-items\n"+
				`[{"key":"bare","content":"No evidence"},{"key":"bound","content":"Has evidence","evidence_paths":["x.go"]}]`+
				"\n```\n"+session.PlanReadyMarker)
		require.NoError(t, coord.ApprovePlan(t.Context(), sess.ID))
		stored, err := env.sessions.Get(t.Context(), sess.ID)
		require.NoError(t, err)
		require.Len(t, stored.Todos, 1)
		require.Equal(t, "bound", stored.Todos[0].Key)
	})

	t.Run("malformed items block degrades to gate-only resolution", func(t *testing.T) {
		t.Parallel()
		coord, env, sess := approveEnv(t)
		writePlanMessage(t, env, sess.ID,
			session.PlanStartMarker+"\n```crush-plan-items\n[{oops]\n```\n"+session.PlanReadyMarker)
		require.NoError(t, coord.ApprovePlan(t.Context(), sess.ID))
		stored, err := env.sessions.Get(t.Context(), sess.ID)
		require.NoError(t, err)
		require.Empty(t, stored.Todos)
	})

	t.Run("dependency cycle fails validation and seeds nothing", func(t *testing.T) {
		t.Parallel()
		coord, env, sess := approveEnv(t)
		writePlanMessage(t, env, sess.ID,
			session.PlanStartMarker+"\n```crush-plan-items\n"+
				`[{"key":"a","content":"A","depends_on":["b"],"evidence_paths":["a.go"]},`+
				`{"key":"b","content":"B","depends_on":["a"],"evidence_paths":["b.go"]}]`+
				"\n```\n"+session.PlanReadyMarker)
		require.NoError(t, coord.ApprovePlan(t.Context(), sess.ID))
		stored, err := env.sessions.Get(t.Context(), sess.ID)
		require.NoError(t, err)
		require.Empty(t, stored.Todos, "a cyclic plan must not seed items that can never be ready")
		// The approval still happened — the gate resolves regardless.
		coord.scopeGate.mu.Lock()
		require.True(t, coord.scopeGate.approved[sess.ID])
		coord.scopeGate.mu.Unlock()
	})

	t.Run("dep on an evidence-dropped item is pruned, not fatal", func(t *testing.T) {
		t.Parallel()
		coord, env, sess := approveEnv(t)
		writePlanMessage(t, env, sess.ID,
			session.PlanStartMarker+"\n```crush-plan-items\n"+
				`[{"key":"bare","content":"No evidence"},{"key":"work","content":"Real work","depends_on":["bare"],"evidence_paths":["w.go"]}]`+
				"\n```\n"+session.PlanReadyMarker)
		require.NoError(t, coord.ApprovePlan(t.Context(), sess.ID))
		stored, err := env.sessions.Get(t.Context(), sess.ID)
		require.NoError(t, err)
		require.Len(t, stored.Todos, 1)
		require.Equal(t, "work", stored.Todos[0].Key)
		require.Empty(t, stored.Todos[0].DependsOn,
			"the edge onto the dropped item prunes like a parse-stage drop")
	})

	t.Run("unknown check names are stripped; paths keep the item", func(t *testing.T) {
		t.Parallel()
		coord, env, sess := approveEnv(t)
		cfg := coord.cfg.Config()
		cfg.Verify = []config.VerifyConfig{{Name: "unit-tests", Command: "go test ./..."}}
		writePlanMessage(t, env, sess.ID,
			session.PlanStartMarker+"\n```crush-plan-items\n"+
				`[{"key":"a","content":"Bound","evidence_checks":["verify:unit-tests","bogus-check"],"evidence_paths":["a.go"]},`+
				`{"key":"b","content":"Only bogus","evidence_checks":["bogus-check"]}]`+
				"\n```\n"+session.PlanReadyMarker)
		require.NoError(t, coord.ApprovePlan(t.Context(), sess.ID))
		stored, err := env.sessions.Get(t.Context(), sess.ID)
		require.NoError(t, err)
		require.Len(t, stored.Todos, 1)
		require.Equal(t, "a", stored.Todos[0].Key)
		require.Equal(t, []string{"verify:unit-tests"}, stored.Todos[0].EvidenceChecks)
	})

	t.Run("busy session rejects approval", func(t *testing.T) {
		t.Parallel()
		coord, env, sess := approveEnv(t)
		stub := &busyStubAgent{sessionBusy: true}
		coord.agents = map[string]SessionAgent{"coder": stub}
		coord.mainAgent = stub
		writePlanMessage(t, env, sess.ID, planWithItems)
		require.ErrorIs(t, coord.ApprovePlan(t.Context(), sess.ID), ErrSessionBusy)
	})
}

// busyStubAgent is a minimal SessionAgent whose only behavior is
// reporting session busyness — enough to reach ApprovePlan's guard.
type busyStubAgent struct {
	sessionBusy bool
}

func (s *busyStubAgent) Run(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
	return nil, nil
}
func (s *busyStubAgent) BeginAccepted(string) *AcceptedRun             { return nil }
func (s *busyStubAgent) SetModels(Model, Model)                        {}
func (s *busyStubAgent) SetTools([]fantasy.AgentTool)                  {}
func (s *busyStubAgent) SetSystemPrompt(prompt.BuiltPrompt)            {}
func (s *busyStubAgent) Cancel(string)                                 {}
func (s *busyStubAgent) CancelAll()                                    {}
func (s *busyStubAgent) IsSessionBusy(string) bool                     { return s.sessionBusy }
func (s *busyStubAgent) IsBusy() bool                                  { return s.sessionBusy }
func (s *busyStubAgent) QueuedPrompts(string) int                      { return 0 }
func (s *busyStubAgent) QueuedPromptsList(string) []string             { return nil }
func (s *busyStubAgent) ClearQueue(string)                             {}
func (s *busyStubAgent) Model() Model                                  { return Model{} }
func (s *busyStubAgent) GenerateTitle(context.Context, string, string) {}
func (s *busyStubAgent) Summarize(context.Context, string, fantasy.ProviderOptions, func(context.Context, *fantasy.ProviderError) error) error {
	return nil
}
