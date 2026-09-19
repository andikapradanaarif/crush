package agent

import (
	"context"
	"fmt"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// fakeQuestionService returns a canned answer selection per Ask call.
type fakeQuestionService struct {
	selected []string
	err      error
	asks     int
	texts    []string
}

func (f *fakeQuestionService) Subscribe(context.Context) <-chan pubsub.Event[question.Request] {
	return nil
}

func (f *fakeQuestionService) SubscribeNotifications(context.Context) <-chan pubsub.Event[question.Notification] {
	return nil
}

func (f *fakeQuestionService) Ask(_ context.Context, req question.Request) ([]question.Answer, error) {
	f.asks++
	for _, q := range req.Questions {
		f.texts = append(f.texts, q.Text)
	}
	if f.err != nil {
		return nil, f.err
	}
	return []question.Answer{{SelectedIDs: f.selected}}, nil
}

func (f *fakeQuestionService) Answer([]question.Answer) bool { return false }
func (f *fakeQuestionService) Cancel() bool                  { return false }

// countingSessionService wraps a real session.Service and counts
// Get calls — the gate's stored-plan check must not read the session
// once the gate has resolved.
type countingSessionService struct {
	session.Service
	gets int
}

func (c *countingSessionService) Get(ctx context.Context, id string) (session.Session, error) {
	c.gets++
	return c.Service.Get(ctx, id)
}

func TestIsMutatingCall(t *testing.T) {
	t.Parallel()
	bash := func(cmd string) fantasy.ToolCall {
		return fantasy.ToolCall{Name: "bash", Input: fmt.Sprintf(`{"command":%q}`, cmd)}
	}
	tests := []struct {
		name string
		call fantasy.ToolCall
		want bool
	}{
		{"write tool", fantasy.ToolCall{Name: "edit"}, true},
		{"read tool", fantasy.ToolCall{Name: "view"}, false},
		{"rm", bash("rm -rf dist"), true},
		{"sed -i", bash(`sed -i 's/a/b/' f.go`), true},
		{"sed -n -i", bash(`sed -n -i 's/a/b/' f.go`), true},
		{"sed --in-place", bash(`sed --in-place 's/a/b/' f.go`), true},
		{"sed -ie bundled", bash(`sed -ie 's/a/b/' f.go`), true},
		{"sed stream-only", bash(`sed -n 's/a/b/p' f.go`), false},
		{"git commit", bash("git commit -m x"), true},
		{"git config --get", bash("git config --get user.name"), false},
		{"git fetch", bash("git fetch origin"), false},
		{"git worktree list", bash("git worktree list"), false},
		{"git branch -D", bash("git branch -D old"), true},
		{"git branch list", bash("git branch"), false},
		{"git update-ref", bash("git update-ref HEAD abc123"), true},
		{"download tool", fantasy.ToolCall{Name: "download"}, true},
		{"kubectl delete", bash("kubectl delete pod x"), true},
		{"kubectl get", bash("kubectl get pods"), false},
		{"apt-get remove", bash("apt-get remove pkg"), true},
		{"rsync", bash("rsync -a src dst"), true},
		{"scp", bash("scp f host:/tmp"), true},
		{"redirect to file", bash("go build -o /dev/null && go test > out.log ./..."), true},
		{"redirect to dev null", bash("go test ./... > /dev/null"), false},
		{"stderr dup", bash("go test ./... 2>&1"), false},
		{"quoted redirect", bash(`echo "progress > bar"`), false},
		{"quoted redirect target", bash(`echo x > 'out'`), true},
		{"stderr-and-stdout to file", bash("go build >& out.log"), true},
		{"variable redirect target still mutates", bash("cmd > $OUT"), true},
		{"tilde redirect target still mutates", bash("cmd > ~/out"), true},
		{"glob redirect target still mutates", bash("cmd > *.log"), true},
		{"substitution redirect target still mutates", bash("cmd > $(gen)"), true},
		{"test comparison is not a write", bash("[[ $a > b.go ]]"), false},
		{"arithmetic is not a write", bash("echo $((a > b))"), false},
		{"escaped quote keeps real redirect", bash(`echo "a \"b" > out`), true},
		{"go test", bash("go test ./..."), false},
		{"make test", bash("make test"), false},
		{"empty input", fantasy.ToolCall{Name: "bash"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, tools.IsMutatingCall(tc.call.Name, tc.call.Input))
		})
	}
}

// gateCtx stamps the session and run identity the gate keys on.
func gateCtx(sessionID string, stamp uint64) context.Context {
	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, sessionID)
	return context.WithValue(ctx, tools.RunStampContextKey, stamp)
}

func exploreN(t *testing.T, ctx context.Context, tool fantasy.AgentTool, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "e", Name: "view"})
		require.NoError(t, err)
		require.False(t, resp.IsError)
	}
}

func TestScopeGate(t *testing.T) {
	t.Parallel()

	newGate := func(t *testing.T, selected []string) (*fakeQuestionService, *fakeTool, fantasy.AgentTool, fantasy.AgentTool) {
		svc := &fakeQuestionService{selected: selected}
		write := &fakeTool{name: "edit", resp: fantasy.NewTextResponse("edited")}
		read := &fakeTool{name: "view", resp: fantasy.NewTextResponse("file contents")}
		wrapped := newScopeGate(svc, true, nil).wrap([]fantasy.AgentTool{read, write})
		return svc, write, wrapped[0], wrapped[1]
	}

	t.Run("write before the threshold passes un-gated", func(t *testing.T) {
		t.Parallel()
		svc, write, readTool, writeTool := newGate(t, []string{"proceed"})
		ctx := gateCtx("s1", 1)
		exploreN(t, ctx, readTool, scopeGateMinExploration-1)
		resp, err := writeTool.Run(ctx, fantasy.ToolCall{ID: "w", Name: "edit"})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.True(t, write.called)
		require.Equal(t, 0, svc.asks)
	})

	t.Run("first write after deep exploration asks once", func(t *testing.T) {
		t.Parallel()
		svc, write, readTool, writeTool := newGate(t, []string{"proceed"})
		ctx := gateCtx("s1", 1)
		exploreN(t, ctx, readTool, scopeGateMinExploration)
		resp, err := writeTool.Run(ctx, fantasy.ToolCall{ID: "w", Name: "edit"})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.True(t, write.called)
		require.Equal(t, 1, svc.asks)

		// Resolved for the rest of the run — no second question.
		resp, err = writeTool.Run(ctx, fantasy.ToolCall{ID: "w2", Name: "edit"})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.Equal(t, 1, svc.asks)
	})

	t.Run("new run re-arms the gate", func(t *testing.T) {
		t.Parallel()
		svc, write, readTool, writeTool := newGate(t, []string{"narrow"})
		// A prior resolved run does not carry its exploration count
		// forward — the boundary is per run.
		exploreN(t, gateCtx("s1", 1), readTool, scopeGateMinExploration)
		ctx := gateCtx("s1", 2)
		exploreN(t, ctx, readTool, scopeGateMinExploration)
		resp, err := writeTool.Run(ctx, fantasy.ToolCall{ID: "w", Name: "edit"})
		require.NoError(t, err)
		require.True(t, resp.IsError, "narrow answer must not execute the write")
		require.False(t, write.called)
		require.Equal(t, 1, svc.asks)
	})

	t.Run("the question reports the real exploration count", func(t *testing.T) {
		t.Parallel()
		svc, _, readTool, writeTool := newGate(t, []string{"proceed"})
		ctx := gateCtx("s1", 1)
		exploreN(t, ctx, readTool, scopeGateMinExploration+4)
		_, err := writeTool.Run(ctx, fantasy.ToolCall{ID: "w", Name: "edit"})
		require.NoError(t, err)
		require.Len(t, svc.texts, 1)
		require.Contains(t, svc.texts[0], fmt.Sprintf("explored %d steps", scopeGateMinExploration+4))
	})

	t.Run("mutating bash gates like a write", func(t *testing.T) {
		t.Parallel()
		svc := &fakeQuestionService{selected: []string{"proceed"}}
		bash := &fakeTool{name: "bash", resp: fantasy.NewTextResponse("done")}
		read := &fakeTool{name: "view", resp: fantasy.NewTextResponse("x")}
		wrapped := newScopeGate(svc, true, nil).wrap([]fantasy.AgentTool{read, bash})
		ctx := gateCtx("s1", 1)
		exploreN(t, ctx, wrapped[0], scopeGateMinExploration)

		resp, err := wrapped[1].Run(ctx, fantasy.ToolCall{
			ID: "b", Name: "bash", Input: `{"command":"rm -rf dist"}`,
		})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.Equal(t, 1, svc.asks)
		require.True(t, bash.called)
	})

	t.Run("read-only bash counts as exploration", func(t *testing.T) {
		t.Parallel()
		svc := &fakeQuestionService{selected: []string{"proceed"}}
		bash := &fakeTool{name: "bash", resp: fantasy.NewTextResponse("ok")}
		write := &fakeTool{name: "edit", resp: fantasy.NewTextResponse("edited")}
		wrapped := newScopeGate(svc, true, nil).wrap([]fantasy.AgentTool{bash, write})
		ctx := gateCtx("s1", 1)
		for range scopeGateMinExploration {
			resp, err := wrapped[0].Run(ctx, fantasy.ToolCall{
				ID: "b", Name: "bash", Input: `{"command":"go test ./internal/..."}`,
			})
			require.NoError(t, err)
			require.False(t, resp.IsError)
		}
		require.Equal(t, 0, svc.asks)
		// The accumulated bash exploration arms the gate for the write.
		_, err := wrapped[1].Run(ctx, fantasy.ToolCall{ID: "w", Name: "edit"})
		require.NoError(t, err)
		require.Equal(t, 1, svc.asks)
	})

	t.Run("declared todos satisfy the gate", func(t *testing.T) {
		t.Parallel()
		svc := &fakeQuestionService{}
		write := &fakeTool{name: "edit", resp: fantasy.NewTextResponse("edited")}
		shared := newScopeGate(svc, true, nil).wrap([]fantasy.AgentTool{
			&fakeTool{name: tools.TodosToolName, resp: fantasy.NewTextResponse("ok")},
			&fakeTool{name: "view", resp: fantasy.NewTextResponse("x")},
			write,
		})
		ctx := gateCtx("s1", 1)
		exploreN(t, ctx, shared[1], scopeGateMinExploration)
		_, err := shared[0].Run(ctx, fantasy.ToolCall{ID: "t", Name: tools.TodosToolName,
			Input: `{"todos":[{"content":"fix the gate","status":"pending","evidence_paths":["internal/agent/scope_gate.go"]}]}`})
		require.NoError(t, err)
		resp, err := shared[2].Run(ctx, fantasy.ToolCall{ID: "w", Name: "edit"})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.True(t, write.called)
		require.Equal(t, 0, svc.asks)
	})

	newTodosGate := func(t *testing.T) (*fakeQuestionService, *fakeTool, *fakeTool, fantasy.AgentTool, fantasy.AgentTool, fantasy.AgentTool) {
		svc := &fakeQuestionService{selected: []string{"proceed"}}
		todosTool := &fakeTool{name: tools.TodosToolName, resp: fantasy.NewTextResponse("ok")}
		write := &fakeTool{name: "edit", resp: fantasy.NewTextResponse("edited")}
		read := &fakeTool{name: "view", resp: fantasy.NewTextResponse("x")}
		wrapped := newScopeGate(svc, true, nil).wrap([]fantasy.AgentTool{todosTool, read, write})
		return svc, todosTool, write, wrapped[0], wrapped[1], wrapped[2]
	}
	barePlan := `{"todos":[{"content":"fix the gate","status":"pending"}]}`
	boundPlan := `{"todos":[{"content":"fix the gate","status":"pending","evidence_checks":["verify:build"]}]}`

	t.Run("bare-string plan does not resolve the gate", func(t *testing.T) {
		t.Parallel()
		svc, _, write, todosTool, readTool, writeTool := newTodosGate(t)
		ctx := gateCtx("s1", 1)
		exploreN(t, ctx, readTool, scopeGateMinExploration)

		// Armed gate: the non-validating declaration bounces with the
		// reason instead of running.
		resp, err := todosTool.Run(ctx, fantasy.ToolCall{ID: "t", Name: tools.TodosToolName, Input: barePlan})
		require.NoError(t, err)
		require.True(t, resp.IsError)
		require.Contains(t, resp.Content, "must bind evidence")

		// The write that follows still hits the scope question.
		_, err = writeTool.Run(ctx, fantasy.ToolCall{ID: "w", Name: "edit"})
		require.NoError(t, err)
		require.Equal(t, 1, svc.asks)
		require.True(t, write.called)
	})

	t.Run("empty plan does not resolve the gate", func(t *testing.T) {
		t.Parallel()
		svc, _, _, todosTool, readTool, writeTool := newTodosGate(t)
		ctx := gateCtx("s1", 1)
		exploreN(t, ctx, readTool, scopeGateMinExploration)
		resp, err := todosTool.Run(ctx, fantasy.ToolCall{ID: "t", Name: tools.TodosToolName, Input: `{"todos":[]}`})
		require.NoError(t, err)
		require.True(t, resp.IsError)
		_, err = writeTool.Run(ctx, fantasy.ToolCall{ID: "w", Name: "edit"})
		require.NoError(t, err)
		require.Equal(t, 1, svc.asks)
	})

	t.Run("bound plan resolves the gate after landing", func(t *testing.T) {
		t.Parallel()
		svc, todosFake, write, todosTool, readTool, writeTool := newTodosGate(t)
		ctx := gateCtx("s1", 1)
		exploreN(t, ctx, readTool, scopeGateMinExploration)
		resp, err := todosTool.Run(ctx, fantasy.ToolCall{ID: "t", Name: tools.TodosToolName, Input: boundPlan})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.True(t, todosFake.called, "the write must land before it counts as declared")
		resp, err = writeTool.Run(ctx, fantasy.ToolCall{ID: "w", Name: "edit"})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.True(t, write.called)
		require.Equal(t, 0, svc.asks)
	})

	t.Run("bookkeeping write on an existing bare plan passes", func(t *testing.T) {
		t.Parallel()
		conn, err := db.Connect(t.Context(), t.TempDir())
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		sessions := session.NewService(db.New(conn), conn)
		sess, err := sessions.Create(t.Context(), "test")
		require.NoError(t, err)
		sess.Todos = []session.PlanItem{{ID: "i1", Content: "existing", Status: session.PlanItemPending}}
		_, err = sessions.Save(t.Context(), sess)
		require.NoError(t, err)

		svc := &fakeQuestionService{selected: []string{"proceed"}}
		todosTool := &fakeTool{name: tools.TodosToolName, resp: fantasy.NewTextResponse("ok")}
		read := &fakeTool{name: "view", resp: fantasy.NewTextResponse("x")}
		write := &fakeTool{name: "edit", resp: fantasy.NewTextResponse("e")}
		wrapped := newScopeGate(svc, true, sessions).wrap([]fantasy.AgentTool{todosTool, read, write})
		ctx := gateCtx(sess.ID, 1)
		exploreN(t, ctx, wrapped[1], scopeGateMinExploration)
		resp, err := wrapped[0].Run(ctx, fantasy.ToolCall{ID: "t", Name: tools.TodosToolName,
			Input: `{"todos":[{"content":"existing","status":"in_progress"}]}`})
		require.NoError(t, err)
		require.False(t, resp.IsError, "a bookkeeping update on an existing plan is not a declaration")
		require.True(t, todosTool.called)
		// The gate is still unresolved — the update didn't validate,
		// so a later write still reaches the question.
		_, err = wrapped[2].Run(ctx, fantasy.ToolCall{ID: "w", Name: "edit"})
		require.NoError(t, err)
		require.Equal(t, 1, svc.asks)
	})

	t.Run("resolved gate stops reading the session", func(t *testing.T) {
		t.Parallel()
		conn, err := db.Connect(t.Context(), t.TempDir())
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		sessions := &countingSessionService{Service: session.NewService(db.New(conn), conn)}
		sess, err := sessions.Create(t.Context(), "test")
		require.NoError(t, err)

		svc := &fakeQuestionService{selected: []string{"proceed"}}
		todosTool := &fakeTool{name: tools.TodosToolName, resp: fantasy.NewTextResponse("ok")}
		read := &fakeTool{name: "view", resp: fantasy.NewTextResponse("x")}
		wrapped := newScopeGate(svc, true, sessions).wrap([]fantasy.AgentTool{todosTool, read})
		ctx := gateCtx(sess.ID, 1)
		exploreN(t, ctx, wrapped[1], scopeGateMinExploration)

		// Armed, no plan stored: a bare list bounces (one Get), a
		// bound list lands and resolves (second Get).
		resp, err := wrapped[0].Run(ctx, fantasy.ToolCall{ID: "t1", Name: tools.TodosToolName,
			Input: `{"todos":[{"content":"bare","status":"pending"}]}`})
		require.NoError(t, err)
		require.True(t, resp.IsError)
		resp, err = wrapped[0].Run(ctx, fantasy.ToolCall{ID: "t2", Name: tools.TodosToolName, Input: boundPlan})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		getsAfterResolve := sessions.gets

		// Resolved: bookkeeping calls must not touch the session row.
		resp, err = wrapped[0].Run(ctx, fantasy.ToolCall{ID: "t3", Name: tools.TodosToolName,
			Input: `{"todos":[{"content":"bare","status":"pending"}]}`})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.Equal(t, getsAfterResolve, sessions.gets, "a resolved gate must skip planDeclared")
	})

	t.Run("todos calls do not count as exploration", func(t *testing.T) {
		t.Parallel()
		svc := &fakeQuestionService{selected: []string{"proceed"}}
		todosTool := &fakeTool{name: tools.TodosToolName, resp: fantasy.NewTextResponse("ok")}
		read := &fakeTool{name: "view", resp: fantasy.NewTextResponse("x")}
		write := &fakeTool{name: "edit", resp: fantasy.NewTextResponse("e")}
		wrapped := newScopeGate(svc, true, nil).wrap([]fantasy.AgentTool{todosTool, read, write})
		ctx := gateCtx("s1", 1)
		exploreN(t, ctx, wrapped[1], scopeGateMinExploration-1)
		for i := 0; i < 5; i++ {
			resp, err := wrapped[0].Run(ctx, fantasy.ToolCall{ID: "t", Name: tools.TodosToolName,
				Input: `{"todos":[{"content":"bare","status":"pending"}]}`})
			require.NoError(t, err)
			require.False(t, resp.IsError)
		}
		resp, err := wrapped[2].Run(ctx, fantasy.ToolCall{ID: "w", Name: "edit"})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.True(t, write.called)
		require.Equal(t, 0, svc.asks, "plan bookkeeping is not exploration — the gate stays unarmed")
	})

	t.Run("bounced plans escalate to the scope question after the budget", func(t *testing.T) {
		t.Parallel()
		svc, todosFake, write, todosTool, readTool, writeTool := newTodosGate(t)
		ctx := gateCtx("s1", 1)
		exploreN(t, ctx, readTool, scopeGateMinExploration)

		for i := 0; i < scopeGatePlanBounceBudget; i++ {
			resp, err := todosTool.Run(ctx, fantasy.ToolCall{ID: fmt.Sprintf("t%d", i), Name: tools.TodosToolName, Input: barePlan})
			require.NoError(t, err)
			require.True(t, resp.IsError, "bounce %d must reject the declaration", i)
		}
		require.False(t, todosFake.called, "a bounced declaration never lands")
		require.Equal(t, 0, svc.asks, "bounces must not ask the question")

		// The next non-validating declaration escalates to the real
		// scope question with stuck-loop context — not a fourth
		// bounce, and never a silent pass-through.
		resp, err := todosTool.Run(ctx, fantasy.ToolCall{ID: "t9", Name: tools.TodosToolName, Input: barePlan})
		require.NoError(t, err)
		require.Equal(t, 1, svc.asks)
		require.Contains(t, svc.texts[0], "bounce loop")
		require.False(t, resp.IsError, "a proceed answer lands the declared write")
		require.True(t, todosFake.called)

		resp, err = writeTool.Run(ctx, fantasy.ToolCall{ID: "w", Name: "edit"})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.True(t, write.called)
		require.Equal(t, 1, svc.asks, "the escalation resolved the gate for the run")
	})

	t.Run("bounce budget resets with the run stamp", func(t *testing.T) {
		t.Parallel()
		svc, _, _, todosTool, readTool, _ := newTodosGate(t)
		ctx := gateCtx("s1", 1)
		exploreN(t, ctx, readTool, scopeGateMinExploration)
		for i := 0; i < scopeGatePlanBounceBudget; i++ {
			resp, err := todosTool.Run(ctx, fantasy.ToolCall{ID: fmt.Sprintf("t%d", i), Name: tools.TodosToolName, Input: barePlan})
			require.NoError(t, err)
			require.True(t, resp.IsError)
		}

		// A new turn re-arms the gate: the budget starts over, so the
		// first bare declaration of run 2 bounces rather than
		// escalating on stale counts.
		ctx2 := gateCtx("s1", 2)
		exploreN(t, ctx2, readTool, scopeGateMinExploration)
		resp, err := todosTool.Run(ctx2, fantasy.ToolCall{ID: "t", Name: tools.TodosToolName, Input: barePlan})
		require.NoError(t, err)
		require.True(t, resp.IsError)
		require.Equal(t, 0, svc.asks)
	})

	t.Run("headless escalation proceeds with a logged assumption", func(t *testing.T) {
		t.Parallel()
		todosFake := &fakeTool{name: tools.TodosToolName, resp: fantasy.NewTextResponse("ok")}
		read := &fakeTool{name: "view", resp: fantasy.NewTextResponse("x")}
		wrapped := newScopeGate(nil, false, nil).wrap([]fantasy.AgentTool{todosFake, read})
		ctx := gateCtx("s1", 1)
		exploreN(t, ctx, wrapped[1], scopeGateMinExploration)
		for i := 0; i < scopeGatePlanBounceBudget; i++ {
			resp, err := wrapped[0].Run(ctx, fantasy.ToolCall{ID: fmt.Sprintf("t%d", i), Name: tools.TodosToolName, Input: barePlan})
			require.NoError(t, err)
			require.True(t, resp.IsError)
		}
		// The question nobody can answer degrades the same way as the
		// scope check itself: proceed with a logged assumption rather
		// than bounce forever.
		resp, err := wrapped[0].Run(ctx, fantasy.ToolCall{ID: "t9", Name: tools.TodosToolName, Input: barePlan})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.True(t, todosFake.called)
	})

	t.Run("malformed plan input gets the tool's own error, not the gate's", func(t *testing.T) {
		t.Parallel()
		svc := &fakeQuestionService{selected: []string{"proceed"}}
		todosTool := &fakeTool{name: tools.TodosToolName, resp: fantasy.NewTextErrorResponse("invalid todos payload")}
		write := &fakeTool{name: "edit", resp: fantasy.NewTextResponse("edited")}
		read := &fakeTool{name: "view", resp: fantasy.NewTextResponse("x")}
		wrapped := newScopeGate(svc, true, nil).wrap([]fantasy.AgentTool{todosTool, read, write})
		ctx := gateCtx("s1", 1)
		exploreN(t, ctx, wrapped[1], scopeGateMinExploration)
		resp, err := wrapped[0].Run(ctx, fantasy.ToolCall{ID: "t", Name: tools.TodosToolName, Input: `{not json`})
		require.NoError(t, err)
		require.True(t, resp.IsError)
		require.Contains(t, resp.Content, "invalid todos payload", "the tool's parse error must surface, not a plan rejection")
	})

	t.Run("a rejected plan write does not resolve the gate", func(t *testing.T) {
		t.Parallel()
		svc := &fakeQuestionService{selected: []string{"proceed"}}
		todosTool := &fakeTool{name: tools.TodosToolName, resp: fantasy.NewTextErrorResponse("validation failed")}
		write := &fakeTool{name: "edit", resp: fantasy.NewTextResponse("edited")}
		read := &fakeTool{name: "view", resp: fantasy.NewTextResponse("x")}
		wrapped := newScopeGate(svc, true, nil).wrap([]fantasy.AgentTool{todosTool, read, write})
		ctx := gateCtx("s1", 1)
		exploreN(t, ctx, wrapped[1], scopeGateMinExploration)
		resp, err := wrapped[0].Run(ctx, fantasy.ToolCall{ID: "t", Name: tools.TodosToolName, Input: boundPlan})
		require.NoError(t, err)
		require.True(t, resp.IsError, "the underlying tool error propagates")
		_, err = wrapped[2].Run(ctx, fantasy.ToolCall{ID: "w", Name: "edit"})
		require.NoError(t, err)
		require.Equal(t, 1, svc.asks, "a rejected write must not count as declared")
	})

	t.Run("user stop ends the turn", func(t *testing.T) {
		t.Parallel()
		svc, write, readTool, writeTool := newGate(t, []string{"stop"})
		ctx := gateCtx("s1", 1)
		exploreN(t, ctx, readTool, scopeGateMinExploration)
		resp, err := writeTool.Run(ctx, fantasy.ToolCall{ID: "w", Name: "edit"})
		require.NoError(t, err)
		require.True(t, resp.StopTurn)
		require.False(t, write.called)
		require.Equal(t, 1, svc.asks)
	})

	t.Run("degraded ask lets the write through", func(t *testing.T) {
		t.Parallel()
		svc := &fakeQuestionService{err: context.DeadlineExceeded}
		write := &fakeTool{name: "edit", resp: fantasy.NewTextResponse("edited")}
		read := &fakeTool{name: "view", resp: fantasy.NewTextResponse("x")}
		wrapped := newScopeGate(svc, true, nil).wrap([]fantasy.AgentTool{read, write})
		ctx := gateCtx("s1", 1)
		exploreN(t, ctx, wrapped[0], scopeGateMinExploration)
		resp, err := wrapped[1].Run(ctx, fantasy.ToolCall{ID: "w", Name: "edit"})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.True(t, write.called)
	})

	t.Run("headless degrade proceeds without asking", func(t *testing.T) {
		t.Parallel()
		write := &fakeTool{name: "edit", resp: fantasy.NewTextResponse("edited")}
		read := &fakeTool{name: "view", resp: fantasy.NewTextResponse("x")}
		wrapped := newScopeGate(nil, false, nil).wrap([]fantasy.AgentTool{read, write})
		ctx := gateCtx("s1", 1)
		exploreN(t, ctx, wrapped[0], scopeGateMinExploration)
		resp, err := wrapped[1].Run(ctx, fantasy.ToolCall{ID: "w", Name: "edit"})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.True(t, write.called)
	})

	t.Run("nil service builds no gate", func(t *testing.T) {
		t.Parallel()
		require.Nil(t, newScopeGate(nil, true, nil))
	})

	t.Run("a tool rebuild keeps gate state", func(t *testing.T) {
		t.Parallel()
		// SetTools rebuilds re-wrap the toolset — the same gate must
		// keep its resolved mark so a turn isn't re-asked.
		svc := &fakeQuestionService{selected: []string{"proceed"}}
		gate := newScopeGate(svc, true, nil)
		write := &fakeTool{name: "edit", resp: fantasy.NewTextResponse("edited")}
		read := &fakeTool{name: "view", resp: fantasy.NewTextResponse("x")}
		wrapped := gate.wrap([]fantasy.AgentTool{read, write})
		ctx := gateCtx("s1", 1)
		exploreN(t, ctx, wrapped[0], scopeGateMinExploration)
		_, err := wrapped[1].Run(ctx, fantasy.ToolCall{ID: "w", Name: "edit"})
		require.NoError(t, err)
		require.Equal(t, 1, svc.asks)

		rewrapped := gate.wrap([]fantasy.AgentTool{read, write})
		resp, err := rewrapped[1].Run(ctx, fantasy.ToolCall{ID: "w2", Name: "edit"})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		require.Equal(t, 1, svc.asks)
	})
}
