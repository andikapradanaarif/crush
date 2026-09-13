package agent

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestPendingChecksForEdit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "pkg")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "foo_test.go"), []byte("package pkg"), 0o644))
	plain := filepath.Join(dir, "plain")
	require.NoError(t, os.MkdirAll(plain, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(plain, "bar.go"), []byte("package plain"), 0o644))

	cfgWithVerify := &config.Config{Verify: []config.VerifyConfig{
		{Name: "build", Command: "go build ./...", Timeout: 60},
	}}
	cfgEmpty := &config.Config{}

	tests := []struct {
		name       string
		cfg        *config.Config
		path       string
		lspCovered bool
		want       []string // expected check names
	}{
		{name: "non-source file selects nothing", cfg: cfgWithVerify, path: filepath.Join(dir, "README.md")},
		{name: "source without LSP gets verify commands", cfg: cfgWithVerify, path: filepath.Join(plain, "bar.go"), want: []string{"verify:build"}},
		{name: "source with LSP skips verify commands", cfg: cfgWithVerify, path: filepath.Join(plain, "bar.go"), lspCovered: true, want: nil},
		{name: "no verify config yields no pending", cfg: cfgEmpty, path: filepath.Join(plain, "bar.go"), want: nil},
		{name: "go file in tested dir gets package test", cfg: cfgEmpty, path: filepath.Join(pkgDir, "foo.go"), want: []string{"package-test:pkg"}},
		{name: "tested-dir check also applies with LSP", cfg: cfgEmpty, path: filepath.Join(pkgDir, "foo.go"), lspCovered: true, want: []string{"package-test:pkg"}},
		{name: "file outside working dir gets no package test", cfg: cfgEmpty, path: filepath.Join(t.TempDir(), "x.go"), want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checks := pendingChecksForEdit(tc.cfg, dir, tc.path, tc.lspCovered)
			var got []string
			for _, c := range checks {
				require.Equal(t, message.VerificationPending, c.State)
				require.NotEmpty(t, c.Command)
				got = append(got, c.Check)
			}
			require.Equal(t, tc.want, got)
		})
	}
}

// stepWith builds a fantasy step carrying the given content parts.
func stepWith(finish fantasy.FinishReason, parts ...fantasy.Content) fantasy.StepResult {
	return fantasy.StepResult{
		Response: fantasy.Response{
			Content:      fantasy.ResponseContent(parts),
			FinishReason: finish,
		},
	}
}

func TestScanVerification(t *testing.T) {
	t.Parallel()
	steps := []fantasy.StepResult{
		stepWith(fantasy.FinishReasonToolCalls,
			fantasy.ToolResultContent{
				ToolCallID: "tc-edit",
				ToolName:   "edit",
				Result:     fantasy.ToolResultOutputContentText{Text: "ok"},
				ClientMetadata: `{"verification":[` +
					`{"check":"diagnostics","state":"passed"},` +
					`{"check":"package-test:pkg","state":"pending","command":"go test ./pkg"}]}`,
			},
		),
		stepWith(fantasy.FinishReasonToolCalls,
			fantasy.ToolCallContent{ToolCallID: "tc-bash", ToolName: "bash", Input: `{"command":"go test ./pkg"}`},
			fantasy.ToolResultContent{
				ToolCallID: "tc-bash",
				ToolName:   "bash",
				Result:     fantasy.ToolResultOutputContentText{Text: "ok pkg\nPASS"},
			},
		),
		stepWith(fantasy.FinishReasonStop, fantasy.TextContent{Text: "done"}),
	}

	failed, pending, observed := scanVerification(steps)
	require.Empty(t, failed)
	require.Len(t, pending, 1)
	require.Equal(t, "package-test:pkg", pending[0].check.Check)
	require.Equal(t, "tc-edit", pending[0].toolCallID)
	require.Len(t, observed, 1)
	require.Equal(t, "go test ./pkg", observed[0].command)
	require.False(t, observed[0].isError)
}

func TestRunGateChecks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := &sessionAgent{}

	mkPending := func(check, command string, step int) gateCheckOutcome {
		return gateCheckOutcome{
			toolCallID: "tc-" + check,
			stepIndex:  step,
			check: message.VerificationCheck{
				Check: check, State: message.VerificationPending, Command: command, Timeout: 30,
			},
		}
	}

	t.Run("runs command and records exit code", func(t *testing.T) {
		res := a.runGateChecks(t.Context(), dir, []gateCheckOutcome{
			mkPending("verify:fail", "exit 3", 0),
			mkPending("verify:pass", "echo ok", 0),
		}, nil)
		require.Equal(t, message.VerificationFailed, res["verify:fail"].state)
		require.Equal(t, "exit code 3", res["verify:fail"].detail)
		require.Equal(t, message.VerificationPassed, res["verify:pass"].state)
	})

	t.Run("dedups identical pending checks", func(t *testing.T) {
		res := a.runGateChecks(t.Context(), dir, []gateCheckOutcome{
			mkPending("verify:x", "echo hi", 0),
			mkPending("verify:x", "echo hi", 1),
		}, nil)
		require.Len(t, res, 1)
	})

	t.Run("observed bash run satisfies pending check", func(t *testing.T) {
		res := a.runGateChecks(t.Context(), dir, []gateCheckOutcome{
			mkPending("verify:test", "go test ./pkg", 0),
		}, []observedBash{
			{stepIdx: 2, command: "go test ./pkg", isError: false, output: "PASS"},
		})
		require.Equal(t, message.VerificationPassed, res["verify:test"].state)
	})

	t.Run("observed failure resolves pending as failed", func(t *testing.T) {
		res := a.runGateChecks(t.Context(), dir, []gateCheckOutcome{
			mkPending("verify:test", "go test ./pkg", 0),
		}, []observedBash{
			{stepIdx: 2, command: "go test ./pkg", isError: true, output: "FAIL"},
		})
		require.Equal(t, message.VerificationFailed, res["verify:test"].state)
	})

	t.Run("observed run before the writes does not satisfy", func(t *testing.T) {
		res := a.runGateChecks(t.Context(), dir, []gateCheckOutcome{
			mkPending("verify:test", "echo ran", 5),
		}, []observedBash{
			{stepIdx: 2, command: "echo ran", isError: false},
		})
		// Not satisfied by the earlier run — the command executes
		// harness-side instead.
		require.Equal(t, message.VerificationPassed, res["verify:test"].state)
		require.Equal(t, "ran\n", res["verify:test"].output)
	})

	t.Run("observed run between two writes does not satisfy", func(t *testing.T) {
		// The same check pending on writes at steps 0 and 4 must not be
		// satisfied by a matching bash run at step 2 — the step-4 write
		// is not covered.
		res := a.runGateChecks(t.Context(), dir, []gateCheckOutcome{
			mkPending("verify:test", "echo ran", 0),
			mkPending("verify:test", "echo ran", 4),
		}, []observedBash{
			{stepIdx: 2, command: "echo ran", isError: false},
		})
		require.Equal(t, "ran\n", res["verify:test"].output)
	})

	t.Run("prefix command does not match observed run", func(t *testing.T) {
		res := a.runGateChecks(t.Context(), dir, []gateCheckOutcome{
			mkPending("verify:test", "echo safe", 0),
		}, []observedBash{
			{stepIdx: 2, command: "echo safe && exit 1", isError: true},
		})
		require.Equal(t, message.VerificationPassed, res["verify:test"].state)
	})
}

func TestMergeVerificationResolved(t *testing.T) {
	t.Parallel()
	existing := `{"hook":{"allow":true},"verification":[{"check":"diagnostics","state":"passed"},{"check":"verify:x","state":"pending"}]}`
	merged := mergeVerificationResolved(existing, []message.VerificationCheck{
		{Check: "verify:x", State: message.VerificationFailed, Detail: "exit code 1"},
	})
	require.Contains(t, merged, `"allow":true`)
	require.Contains(t, merged, `"check":"verify:x","state":"failed"`)
	require.NotContains(t, merged, "pending")
}

func TestUnionToolMetadata(t *testing.T) {
	t.Parallel()
	stored := `{"verification":[{"check":"verify:x","state":"failed"}]}`
	incoming := `{"hook":{"deny":false}}`
	merged := unionToolMetadata(stored, incoming)
	require.Contains(t, merged, `"verification"`)
	require.Contains(t, merged, `"hook"`)

	// Incoming wins on conflicts.
	merged = unionToolMetadata(`{"verification":[{"state":"pending"}]}`, `{"verification":[{"state":"passed"}]}`)
	require.Contains(t, merged, "passed")
}

// newGateTestAgent builds a sessionAgent with the pieces the gate
// touches: config store, message service, and the queue/dispatch maps.
func newGateTestAgent(t *testing.T, cfg *config.Config) (*sessionAgent, message.Service, string) {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	sess, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)

	svc := message.NewService(q)
	return &sessionAgent{
		configStore:  config.NewTestStore(cfg),
		messages:     svc,
		messageQueue: csync.NewMap[string, []SessionAgentCall](),
		dispatchMu:   csync.NewMap[string, *sync.Mutex](),
	}, svc, sess.ID
}

func TestRunVerificationGate(t *testing.T) {
	t.Parallel()

	editWith := func(meta string) fantasy.ToolResultContent {
		return fantasy.ToolResultContent{
			ToolCallID:     "tc-edit",
			ToolName:       "edit",
			Result:         fantasy.ToolResultOutputContentText{Text: "edited"},
			ClientMetadata: meta,
		}
	}
	assistantMsg := func() *message.Message {
		return &message.Message{Role: message.Assistant}
	}

	t.Run("failed decorator verdict queues a retry", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		result := &fantasy.AgentResult{Steps: []fantasy.StepResult{
			stepWith(fantasy.FinishReasonToolCalls, editWith(`{"verification":[{"check":"diagnostics","state":"failed","detail":"2 new error(s)"}]}`)),
			stepWith(fantasy.FinishReasonStop, fantasy.TextContent{Text: "done"}),
		}}

		queued := a.runVerificationGate(t.Context(), SessionAgentCall{
			SessionID: sessionID, RunID: "run-1", Prompt: "do it",
		}, result, assistantMsg())
		require.True(t, queued)
		q, ok := a.messageQueue.Get(sessionID)
		require.True(t, ok)
		require.Len(t, q, 1)
		require.Equal(t, "run-1", q[0].RunID)
		require.Equal(t, 1, q[0].VerificationAttempts)
		require.Contains(t, q[0].Prompt, "diagnostics")
	})

	t.Run("stop-turn ending does not gate", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		result := &fantasy.AgentResult{Steps: []fantasy.StepResult{
			stepWith(fantasy.FinishReasonToolCalls, editWith(`{"verification":[{"check":"diagnostics","state":"failed"}]}`)),
			stepWith(fantasy.FinishReasonStop, fantasy.ToolResultContent{
				ToolCallID: "tc-q", ToolName: "question", StopTurn: true,
				Result: fantasy.ToolResultOutputContentText{Text: "denied"},
			}),
		}}
		queued := a.runVerificationGate(t.Context(), SessionAgentCall{SessionID: sessionID}, result, assistantMsg())
		require.False(t, queued)
	})

	t.Run("non-stop terminal step does not gate", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		result := &fantasy.AgentResult{Steps: []fantasy.StepResult{
			stepWith(fantasy.FinishReasonToolCalls, editWith(`{"verification":[{"check":"diagnostics","state":"failed"}]}`)),
			stepWith(fantasy.FinishReasonLength, fantasy.TextContent{Text: "cut off"}),
		}}
		require.False(t, a.runVerificationGate(t.Context(), SessionAgentCall{SessionID: sessionID}, result, assistantMsg()))
	})

	t.Run("observed failing bash resolves pending and retries", func(t *testing.T) {
		t.Parallel()
		a, svc, sessionID := newGateTestAgent(t, &config.Config{})
		// The stored tool-result row the outcome lands on.
		mkMsg(t, svc, sessionID, message.Tool, message.ToolResult{
			ToolCallID: "tc-edit", Name: "edit", Content: "edited",
			Metadata: `{"verification":[{"check":"verify:test","state":"pending","command":"go test ./x"}]}`,
		})
		result := &fantasy.AgentResult{Steps: []fantasy.StepResult{
			stepWith(fantasy.FinishReasonToolCalls, editWith(`{"verification":[{"check":"verify:test","state":"pending","command":"go test ./x"}]}`)),
			stepWith(fantasy.FinishReasonToolCalls,
				fantasy.ToolCallContent{ToolCallID: "tc-bash", ToolName: "bash", Input: `{"command":"go test ./x"}`},
				fantasy.ToolResultContent{
					ToolCallID: "tc-bash", ToolName: "bash",
					Result: fantasy.ToolResultOutputContentError{Error: errTest},
				},
			),
			stepWith(fantasy.FinishReasonStop, fantasy.TextContent{Text: "done"}),
		}}

		queued := a.runVerificationGate(t.Context(), SessionAgentCall{SessionID: sessionID, RunID: "r"}, result, assistantMsg())
		require.True(t, queued)

		// The stored row's pending entry resolved to failed.
		msgs, err := svc.List(t.Context(), sessionID)
		require.NoError(t, err)
		require.Contains(t, msgs[0].ToolResults()[0].Metadata, `"state":"failed"`)
	})

	t.Run("exhausted budget surfaces on the assistant message", func(t *testing.T) {
		t.Parallel()
		a, _, sessionID := newGateTestAgent(t, &config.Config{})
		asst := assistantMsg()
		result := &fantasy.AgentResult{Steps: []fantasy.StepResult{
			stepWith(fantasy.FinishReasonToolCalls, editWith(`{"verification":[{"check":"diagnostics","state":"failed","detail":"1 new error(s)"}]}`)),
			stepWith(fantasy.FinishReasonStop, fantasy.TextContent{Text: "done"}),
		}}
		queued := a.runVerificationGate(t.Context(), SessionAgentCall{
			SessionID: sessionID, VerificationAttempts: maxVerificationAttempts,
		}, result, asst)
		require.False(t, queued)
		require.Contains(t, asst.Content().Text, "still failing")
	})
}

var errTest = errors.New("exit code 1")
