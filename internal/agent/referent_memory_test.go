package agent

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/params"
	"github.com/stretchr/testify/require"
)

// settleReferents forces every pending episode's window closed
// (settle_at=0 mimics the +24h cap elapsing) and runs one settle
// pass — the path promotion now takes, since #340 counts frozen
// outcomes rather than raw cues.
func settleReferents(t *testing.T, env fakeEnv, svc cmdlog.Service) {
	t.Helper()
	_, err := env.conn.ExecContext(t.Context(),
		`UPDATE referent_episodes SET settle_at = 0 WHERE settled_at IS NULL`)
	require.NoError(t, err)
	require.NoError(t, svc.SettleReferentLabels(t.Context(), 50))
}

func referentUserMsg(id, text string) message.Message {
	return message.Message{
		ID:    id,
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: text}},
	}
}

// msgIdx locates a fixture message by ID — toolOK inserts result
// messages, so positions can't stay numeric literals.
func msgIdx(msgs []message.Message, id string) int {
	for i, m := range msgs {
		if m.ID == id {
			return i
		}
	}
	return -1
}

// toolOK stamps a clean ToolResult for every tool call in the
// fixture — the shape a real transcript has, where a finished call's
// result rides a tool-role message behind the assistant's. Turn and
// referent evidence counts only executed calls (#358), so a fixture
// call that must count must show its result.
func toolOK(msgs []message.Message) []message.Message {
	var out []message.Message
	for _, m := range msgs {
		out = append(out, m)
		if m.Role != message.Assistant {
			continue
		}
		for _, tc := range m.ToolCalls() {
			out = append(out, message.Message{
				Role: message.Tool,
				Parts: []message.ContentPart{message.ToolResult{
					ToolCallID: tc.ID, Name: tc.Name, Content: "ok",
				}},
			})
		}
	}
	return out
}

func referentEditMsg(callID, tool, path string) message.Message {
	return message.Message{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{
				ID:       callID,
				Name:     tool,
				Input:    fmt.Sprintf(`{"file_path":%q}`, path),
				Finished: true,
			},
			// The result rides the same fixture message —
			// ToolResults() scans parts regardless of role, and a
			// call counts as evidence only with a clean result.
			message.ToolResult{ToolCallID: callID, Name: tool, Content: "ok"},
		},
	}
}

// referentAgent wires a sessionAgent with a real cmdlog service over a
// real db — the end-to-end cases need episode rows to actually land.
// The fakeEnv comes back so tests can seed child-session messages.
func referentAgent(t *testing.T) (*sessionAgent, fakeEnv) {
	t.Helper()
	env := testEnv(t)
	a := &sessionAgent{
		configStore: config.NewTestStoreWithDir(&config.Config{}, env.workingDir),
		sessions:    env.sessions,
		messages:    env.messages,
		cmdlog:      env.cmdlog,
	}
	return a, env
}

func TestExtractReferentPhrase(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		prompt string
		want   string
	}{
		{"fix the test", "test"},
		{"update the Config", "config"},
		{"fix the test and the config", ""},                // two referents: unknowable mapping
		{"fix the test twice, the test again", "test"},     // same noun repeated
		{"fix it", ""},                                     // pronoun: no learnable phrase
		{"do the thing", ""},                               // junk noun: no target to learn
		{"fix the same thing", ""},                         // junk nouns never mint phrases
		{"update the config and the same thing", "config"}, // junk filtered, real noun survives
		{"rerun", ""},
	} {
		t.Run(tc.prompt, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, extractReferentPhrase(tc.prompt))
		})
	}
}

func TestReferentJudgedVerdict(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		prompt string
		want   string
	}{
		{"thanks", cmdlog.ReferentAccepted},
		{"now add a README", cmdlog.ReferentAccepted},
		{"also update config.go", cmdlog.ReferentAccepted}, // names target, no revision cue
		{"wrong file", cmdlog.ReferentRevised},
		{"wrong.", cmdlog.ReferentRevised},
		{"incorrect", cmdlog.ReferentRevised},
		{"nope", cmdlog.ReferentRevised},
		{"try again", cmdlog.ReferentRevised},
		{"redo it", cmdlog.ReferentRevised},
		{"start over", cmdlog.ReferentRevised},
		{"revert that", cmdlog.ReferentRevised},
		{"that's not what I meant", cmdlog.ReferentRevised},
		{"oops, undo it", cmdlog.ReferentRevised},
		// A follow-up the English cues cannot read is unknown —
		// unparseable input fails closed, never mints acceptance.
		{"改错了文件", cmdlog.ReferentUnknown},
		{"修正してください", cmdlog.ReferentUnknown},
		{"👍", cmdlog.ReferentUnknown},
		{"!!!", cmdlog.ReferentUnknown},
		{"", cmdlog.ReferentUnknown},
		// Non-ASCII punctuation inside readable English still
		// classifies — only letters outside ASCII mark unknown.
		{"looks good — ship it", cmdlog.ReferentAccepted},
	} {
		t.Run(tc.prompt, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, referentJudgedVerdict(tc.prompt))
		})
	}
}

func TestReferentTargets(t *testing.T) {
	t.Parallel()
	a, env := referentAgent(t)
	abs := filepath.Join(env.workingDir, "internal", "cfg.go")

	msgs := []message.Message{
		referentUserMsg("u1", "fix the config"),
		// A view is exploration, not commitment — excluded.
		{Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{
			ID: "tc-view", Name: tools.ViewToolName, Input: `{"file_path":"v.go"}`, Finished: true,
		}}},
		referentEditMsg("tc1", tools.EditToolName, abs),
		referentEditMsg("tc2", tools.EditToolName, abs), // duplicate path collapses
		referentEditMsg("tc3", tools.WriteToolName, "other.go"),
		// Malformed input is skipped, not fatal.
		{Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{
			ID: "tc-bad", Name: tools.EditToolName, Input: "not json", Finished: true,
		}}},
	}
	targets := a.referentTargets(t.Context(), msgs, 1)
	require.Len(t, targets, 2)
	// Stored paths are slash-normalized (relWorkdir's ToSlash) —
	// filepath.Join would assert a backslash on Windows.
	require.Equal(t, "internal/cfg.go", targets[0].path)
	require.Equal(t, "tc1", targets[0].callID)
	require.Equal(t, "other.go", targets[1].path)
	require.Equal(t, "tc3", targets[1].callID)
}

func TestRecordReferentEpisodes(t *testing.T) {
	t.Parallel()

	t.Run("accepted vague turn records a clean episode", func(t *testing.T) {
		t.Parallel()
		a, env := referentAgent(t)
		svc := env.cmdlog
		target := filepath.Join(env.workingDir, "x.go")
		msgs := []message.Message{
			referentUserMsg("u1", "fix the config"),
			referentEditMsg("tc1", tools.EditToolName, target),
		}
		a.recordReferentEpisodes(t.Context(), SessionAgentCall{
			SessionID: "s1", Prompt: "looks good",
		}, msgs, nil)

		refs, err := svc.ListReferentCandidates(t.Context(), []string{"config"}, 5)
		require.NoError(t, err)
		require.Empty(t, refs) // one clean session is below the default floor

		// A second session re-deriving the same mapping crosses the
		// promotion floor — once both episodes' outcomes freeze.
		a.recordReferentEpisodes(t.Context(), SessionAgentCall{
			SessionID: "s2", Prompt: "ok",
		}, []message.Message{
			referentUserMsg("u2", "fix the config"),
			referentEditMsg("tc2", tools.EditToolName, target),
		}, nil)
		// Promotion consumes settled outcomes only: a fresh row can't
		// count itself, so the crossing happens at settlement (no
		// git/fingerprints → the accepted cue is the frozen outcome).
		settleReferents(t, env, svc)
		refs, err = svc.ListReferentCandidates(t.Context(), []string{"config"}, 5)
		require.NoError(t, err)
		require.Len(t, refs, 1)
		require.Equal(t, "x.go", refs[0].Target)
	})

	t.Run("revision cue records but does not promote", func(t *testing.T) {
		t.Parallel()
		a, env := referentAgent(t)
		svc := env.cmdlog
		target := filepath.Join(env.workingDir, "x.go")
		a.recordReferentEpisodes(t.Context(), SessionAgentCall{
			SessionID: "s1", Prompt: "wrong file, revert it",
		}, []message.Message{
			referentUserMsg("u1", "fix the config"),
			referentEditMsg("tc1", tools.EditToolName, target),
		}, nil)
		a.recordReferentEpisodes(t.Context(), SessionAgentCall{
			SessionID: "s2", Prompt: "fine",
		}, []message.Message{
			referentUserMsg("u2", "fix the config"),
			referentEditMsg("tc2", tools.EditToolName, target),
		}, nil)
		// One revised + one accepted: the revised episode still doesn't
		// count, so one clean session stays below the floor.
		refs, err := svc.ListReferentCandidates(t.Context(), []string{"config"}, 5)
		require.NoError(t, err)
		require.Empty(t, refs)
	})

	t.Run("suggested target is contaminated evidence", func(t *testing.T) {
		t.Parallel()
		a, env := referentAgent(t)
		svc := env.cmdlog
		target := filepath.Join(env.workingDir, "x.go")
		// The tail rendered x.go as a referent target this session —
		// an edit landing on it is echo, not evidence.
		svc.MarkSuggestedFile("s1", "x.go")
		for _, sess := range []string{"s1", "s2"} {
			svc.MarkSuggestedFile(sess, "x.go")
			a.recordReferentEpisodes(t.Context(), SessionAgentCall{
				SessionID: sess, Prompt: "ok",
			}, []message.Message{
				referentUserMsg("u-"+sess, "fix the config"),
				referentEditMsg("tc-"+sess, tools.EditToolName, target),
			}, nil)
		}
		refs, err := svc.ListReferentCandidates(t.Context(), []string{"config"}, 5)
		require.NoError(t, err)
		require.Empty(t, refs)
	})

	t.Run("explicit-path prompt is not vague and records nothing", func(t *testing.T) {
		t.Parallel()
		a, env := referentAgent(t)
		svc := env.cmdlog
		target := filepath.Join(env.workingDir, "x.go")
		a.recordReferentEpisodes(t.Context(), SessionAgentCall{
			SessionID: "s1", Prompt: "ok",
		}, []message.Message{
			referentUserMsg("u1", "fix "+target),
			referentEditMsg("tc1", tools.EditToolName, target),
		}, nil)
		refs, err := svc.ListReferentCandidates(t.Context(), []string{"config"}, 5)
		require.NoError(t, err)
		require.Empty(t, refs)
	})

	// A denied edit's result is IsError — the call never ran, so a
	// vague prompt plus a denied mutation mints no episode (#358).
	t.Run("denied edit records nothing", func(t *testing.T) {
		t.Parallel()
		a, env := referentAgent(t)
		svc := env.cmdlog
		target := filepath.Join(env.workingDir, "x.go")
		a.recordReferentEpisodes(t.Context(), SessionAgentCall{
			SessionID: "s1", Prompt: "ok",
		}, []message.Message{
			referentUserMsg("u1", "fix the config"),
			{Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{
				ID: "tc1", Name: tools.EditToolName,
				Input:    fmt.Sprintf(`{"file_path":%q,"new_string":"x"}`, target),
				Finished: true,
			}}},
			{Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{
				ToolCallID: "tc1", Name: tools.EditToolName,
				Content: "User denied permission", IsError: true,
			}}},
		}, nil)
		refs, err := svc.ListReferentCandidates(t.Context(), []string{"config"}, 5)
		require.NoError(t, err)
		require.Empty(t, refs)
	})

	t.Run("multi-referent prompt records nothing", func(t *testing.T) {
		t.Parallel()
		a, env := referentAgent(t)
		svc := env.cmdlog
		target := filepath.Join(env.workingDir, "x.go")
		a.recordReferentEpisodes(t.Context(), SessionAgentCall{
			SessionID: "s1", Prompt: "ok",
		}, []message.Message{
			referentUserMsg("u1", "fix the config and the test"),
			referentEditMsg("tc1", tools.EditToolName, target),
		}, nil)
		refs, err := svc.ListReferentCandidates(t.Context(), []string{"config", "test"}, 5)
		require.NoError(t, err)
		require.Empty(t, refs)
	})

	t.Run("multi-target turn abstains", func(t *testing.T) {
		t.Parallel()
		a, env := referentAgent(t)
		// promoteMin 1 makes a single recorded episode promote —
		// an empty result here proves nothing was recorded at all.
		a.memParams = params.DefaultMemory()
		a.memParams.ReferentPromoteHits = 1
		a.recordReferentEpisodes(t.Context(), SessionAgentCall{
			SessionID: "s1", Prompt: "ok",
		}, []message.Message{
			referentUserMsg("u1", "fix the config"),
			referentEditMsg("tc1", tools.EditToolName, filepath.Join(env.workingDir, "x.go")),
			// A collateral edit on a second file makes phrase→target
			// attribution unknowable — record neither.
			referentEditMsg("tc2", tools.EditToolName, filepath.Join(env.workingDir, "y.go")),
		}, nil)
		refs, err := env.cmdlog.ListReferentCandidates(t.Context(), []string{"config"}, 5)
		require.NoError(t, err)
		require.Empty(t, refs)
	})

	t.Run("repair prompt does not shadow the judged turn", func(t *testing.T) {
		t.Parallel()
		a, env := referentAgent(t)
		a.memParams = params.DefaultMemory()
		a.memParams.ReferentPromoteHits = 1
		// The settle-time floor lives on the service, built at
		// construction — rebuild it with the test's floor of 1.
		mp := params.DefaultMemory()
		mp.ReferentPromoteHits = 1
		env.cmdlog = cmdlog.NewService(db.New(env.conn), env.workingDir, mp)
		a.cmdlog = env.cmdlog
		target := filepath.Join(env.workingDir, "x.go")
		a.recordReferentEpisodes(t.Context(), SessionAgentCall{
			SessionID: "s1", Prompt: "ok",
		}, []message.Message{
			referentUserMsg("u1", "fix the config"),
			referentEditMsg("tc1", tools.EditToolName, target),
			// A failed-verify retry persists its harness prompt as a
			// user row between the vague turn and this judgment —
			// the scan must skip it, not judge the repair prompt.
			referentUserMsg("r1", "Verification failed. exit code 1"),
			referentEditMsg("tc2", tools.EditToolName, target),
		}, nil)
		settleReferents(t, env, env.cmdlog)
		refs, err := env.cmdlog.ListReferentCandidates(t.Context(), []string{"config"}, 5)
		require.NoError(t, err)
		require.Len(t, refs, 1)
		require.Equal(t, "x.go", refs[0].Target)
	})

	t.Run("delegated edit counts toward the judged turn", func(t *testing.T) {
		t.Parallel()
		a, env := referentAgent(t)
		a.memParams = params.DefaultMemory()
		a.memParams.ReferentPromoteHits = 1
		// Same settle-floor rebuild as the repair-prompt case.
		mp := params.DefaultMemory()
		mp.ReferentPromoteHits = 1
		env.cmdlog = cmdlog.NewService(db.New(env.conn), env.workingDir, mp)
		a.cmdlog = env.cmdlog
		target := filepath.Join(env.workingDir, "x.go")

		// The edit lives in the child `agent` session's transcript —
		// the parent messages carry only the agent tool call.
		childID := env.sessions.CreateAgentToolSessionID("a1", "tc-agent")
		_, err := env.sessions.CreateTaskSession(t.Context(), childID, "s1", "task")
		require.NoError(t, err)
		_, err = env.messages.Create(t.Context(), childID, message.CreateMessageParams{
			Role: message.Assistant,
			Parts: []message.ContentPart{message.ToolCall{
				ID: "tc-child", Name: tools.EditToolName,
				Input:    fmt.Sprintf(`{"file_path":%q}`, target),
				Finished: true,
			}},
		})
		require.NoError(t, err)
		_, err = env.messages.Create(t.Context(), childID, message.CreateMessageParams{
			Role: message.Tool,
			Parts: []message.ContentPart{message.ToolResult{
				ToolCallID: "tc-child", Name: tools.EditToolName, Content: "ok",
			}},
		})
		require.NoError(t, err)

		a.recordReferentEpisodes(t.Context(), SessionAgentCall{
			SessionID: "s1", Prompt: "ok",
		}, []message.Message{
			referentUserMsg("u1", "fix the config"),
			{ID: "a1", Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{
				ID: "tc-agent", Name: AgentToolName,
				Input:    `{"prompt":"fix the config"}`,
				Finished: true,
			}}},
		}, nil)
		settleReferents(t, env, env.cmdlog)
		refs, err := env.cmdlog.ListReferentCandidates(t.Context(), []string{"config"}, 5)
		require.NoError(t, err)
		require.Len(t, refs, 1)
		require.Equal(t, "x.go", refs[0].Target)
	})

	// Issue #361: a vague turn followed by compaction leaves no real
	// user message in the post-summary tail — without the
	// full-transcript fallback the judged turn's episode is lost.
	t.Run("post-compaction tail finds the judged turn", func(t *testing.T) {
		t.Parallel()
		a, env := referentAgent(t)
		mp := params.DefaultMemory()
		mp.ReferentPromoteHits = 1
		env.cmdlog = cmdlog.NewService(db.New(env.conn), env.workingDir, mp)
		a.cmdlog = env.cmdlog
		target := filepath.Join(env.workingDir, "x.go")

		sess, err := env.sessions.Create(t.Context(), "s1")
		require.NoError(t, err)
		var vagueID string
		for _, m := range []message.CreateMessageParams{
			{Role: message.User, Parts: []message.ContentPart{
				message.TextContent{Text: "fix the config"},
			}},
			{Role: message.Assistant, Parts: []message.ContentPart{
				message.ToolCall{
					ID: "tc1", Name: tools.EditToolName, Finished: true,
					Input: fmt.Sprintf(`{"file_path":%q,"old_string":"","new_string":"v1"}`, target),
				},
				message.ToolResult{ToolCallID: "tc1", Name: tools.EditToolName, Content: "ok"},
			}},
		} {
			created, err := env.messages.Create(t.Context(), sess.ID, m)
			require.NoError(t, err)
			if m.Role == message.User {
				vagueID = created.ID
			}
		}
		// Backdate the judged prompt so mint-vs-prompt anchoring is
		// observable — the window must cover the turn's aftermath,
		// not the mint moment.
		promptSec := time.Now().Add(-2 * time.Hour).Unix()
		_, err = env.conn.ExecContext(t.Context(),
			`UPDATE messages SET created_at = ? WHERE id = ?`, promptSec, vagueID)
		require.NoError(t, err)
		// Production ordering: the in-flight prompt persists before
		// the record pass reads the transcript — exclude it or the
		// fallback lands on it and judges nothing.
		inflight, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "looks right"}},
		})
		require.NoError(t, err)

		// The tail the live path sees: only the compaction summary.
		// The caller passes the full transcript fetched once for both
		// judgment passes — production reads it at the same point.
		fullTx, err := env.messages.List(t.Context(), sess.ID)
		require.NoError(t, err)
		a.recordReferentEpisodes(t.Context(), SessionAgentCall{
			SessionID: sess.ID, Prompt: "looks right", currentUserMsgID: inflight.ID,
		}, []message.Message{{
			ID: "sum", Role: message.User, IsSummaryMessage: true,
			Parts: []message.ContentPart{message.TextContent{Text: "summary"}},
		}}, fullTx)

		var n int
		require.NoError(t, env.conn.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM referent_episodes
			 WHERE session_id = ? AND source_message_id = ?`,
			sess.ID, vagueID).Scan(&n))
		require.Equal(t, 1, n,
			"the pre-summary vague turn mints its episode via the full transcript")
		// The in-flight prompt is a boundary for the span but records
		// no episode of its own.
		require.NoError(t, env.conn.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM referent_episodes
			 WHERE source_message_id = ?`, inflight.ID).Scan(&n))
		require.Zero(t, n, "no phantom episode keyed to the in-flight prompt")
		var gotTarget, verdict string
		require.NoError(t, env.conn.QueryRowContext(t.Context(),
			`SELECT target, verdict FROM referent_episodes
			 WHERE source_message_id = ?`, vagueID).Scan(&gotTarget, &verdict))
		require.Equal(t, "x.go", gotTarget)
		require.Equal(t, cmdlog.ReferentAccepted, verdict,
			"the in-flight prompt is the verdict source")
		var epCreated, labeledAt int64
		var addedLines string
		require.NoError(t, env.conn.QueryRowContext(t.Context(),
			`SELECT created_at, labeled_at, label_added_lines
			 FROM referent_episodes WHERE source_message_id = ?`,
			vagueID).Scan(&epCreated, &labeledAt, &addedLines))
		require.Equal(t, promptSec*1000, epCreated,
			"created_at anchors at the judged prompt, not the mint")
		require.Greater(t, labeledAt, int64(0),
			"labels landed in the insert — born-due rows never freeze fingerprintless")
		require.NotEmpty(t, addedLines)
	})

	// The last real boundary judges, always: when a real prompt
	// intervened between a vague turn and compaction, the fallback
	// judges THAT turn — the earlier vague turn is unrecorded
	// (referents have no backfill, by design) and the judged span
	// bounds targets to the intervening turn's work alone.
	t.Run("intervening prompt is judged, earlier vague turn unrecorded", func(t *testing.T) {
		t.Parallel()
		a, env := referentAgent(t)
		mp := params.DefaultMemory()
		mp.ReferentPromoteHits = 1
		env.cmdlog = cmdlog.NewService(db.New(env.conn), env.workingDir, mp)
		a.cmdlog = env.cmdlog
		targetA := filepath.Join(env.workingDir, "a.go")
		targetB := filepath.Join(env.workingDir, "b.go")

		sess, err := env.sessions.Create(t.Context(), "s1")
		require.NoError(t, err)
		edit := func(tcID, path string) message.CreateMessageParams {
			return message.CreateMessageParams{
				Role: message.Assistant,
				Parts: []message.ContentPart{
					message.ToolCall{
						ID: tcID, Name: tools.EditToolName, Finished: true,
						Input: fmt.Sprintf(`{"file_path":%q,"old_string":"","new_string":"v1"}`, path),
					},
					message.ToolResult{ToolCallID: tcID, Name: tools.EditToolName, Content: "ok"},
				},
			}
		}
		userMsg := func(text string) message.CreateMessageParams {
			return message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{
				message.TextContent{Text: text},
			}}
		}
		var vagueID, interveningID string
		for i, m := range []message.CreateMessageParams{
			userMsg("fix the config"),
			edit("tc1", targetA),
			userMsg("fix the tests"),
			edit("tc2", targetB),
		} {
			created, err := env.messages.Create(t.Context(), sess.ID, m)
			require.NoError(t, err)
			switch i {
			case 0:
				vagueID = created.ID
			case 2:
				interveningID = created.ID
			}
		}
		inflight, err := env.messages.Create(t.Context(), sess.ID,
			userMsg("looks right"))
		require.NoError(t, err)

		fullTx, err := env.messages.List(t.Context(), sess.ID)
		require.NoError(t, err)
		a.recordReferentEpisodes(t.Context(), SessionAgentCall{
			SessionID: sess.ID, Prompt: "looks right", currentUserMsgID: inflight.ID,
		}, []message.Message{{
			ID: "sum", Role: message.User, IsSummaryMessage: true,
			Parts: []message.ContentPart{message.TextContent{Text: "summary"}},
		}}, fullTx)

		var target, verdict string
		require.NoError(t, env.conn.QueryRowContext(t.Context(),
			`SELECT target, verdict FROM referent_episodes
			 WHERE source_message_id = ?`, interveningID).Scan(&target, &verdict))
		require.Equal(t, "b.go", target,
			"targets are bounded to the judged turn — the earlier turn's a.go edit stays out")
		require.Equal(t, cmdlog.ReferentAccepted, verdict)
		var n int
		require.NoError(t, env.conn.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM referent_episodes
			 WHERE source_message_id = ?`, vagueID).Scan(&n))
		require.Zero(t, n,
			"the earlier vague turn mints no episode — no backfill channel")
	})
}

func TestReferentCandidates_Gating(t *testing.T) {
	t.Parallel()

	promote := func(t *testing.T, env fakeEnv, target string) {
		t.Helper()
		for _, sess := range []string{"s1", "s2"} {
			require.NoError(t, env.cmdlog.RecordReferentEpisode(t.Context(), cmdlog.ReferentEpisode{
				Phrase: "config", Target: target, SessionID: sess,
				SourceMessageID: "m-" + sess, Verdict: cmdlog.ReferentAccepted,
			}, params.DefaultMemory().ReferentPromoteHits))
		}
		// Promotion consumes settled outcomes — freeze both episodes
		// so the second row's success crossing fires at settle time.
		settleReferents(t, env, env.cmdlog)
	}

	t.Run("option off fetches nothing", func(t *testing.T) {
		t.Parallel()
		a, env := referentAgent(t)
		promote(t, env, "x.go")
		refs, _ := a.referentCandidates(t.Context(), SessionAgentCall{
			SessionID: "s3", Prompt: "fix the config",
		}, params.DefaultMemory())
		require.Empty(t, refs)
	})

	t.Run("promoted mapping surfaces on a matching vague prompt", func(t *testing.T) {
		t.Parallel()
		a, env := referentAgent(t)
		a.referentMemory = true
		promote(t, env, "x.go")
		// The promotion floor is this test's subject — the rate
		// gate (#296) gets its own subtests below.
		mp := params.DefaultMemory()
		mp.ReferentRateFloor = 0
		refs, _ := a.referentCandidates(t.Context(), SessionAgentCall{
			SessionID: "s3", Prompt: "fix the config",
		}, mp)
		require.Len(t, refs, 1)
		require.Equal(t, "x.go", refs[0].Target)
		require.EqualValues(t, 2, refs[0].Hits) // seeded with the crossing count, not 1
	})

	t.Run("rate gate suppresses a sparse posterior", func(t *testing.T) {
		t.Parallel()
		a, env := referentAgent(t)
		a.referentMemory = true
		promote(t, env, "x.go")
		// Two accepted episodes sit in the ledger but the leave-
		// one-out prior is still Beta(1,1) — q0.1 = 0.1, well under
		// the 0.6 floor, so a sparse project abstains (#296).
		refs, gate := a.referentCandidates(t.Context(), SessionAgentCall{
			SessionID: "s3", Prompt: "fix the config",
		}, params.DefaultMemory())
		require.Empty(t, refs)
		require.NotNil(t, gate, "a decided gate owes the turn its audit record")
		require.True(t, gate.gated)
		require.InDelta(t, 0.1, gate.bound, 1e-9)
		require.Equal(t, params.DefaultMemory().ReferentRateFloor, gate.floor)
	})

	t.Run("rate gate admits once the bound clears", func(t *testing.T) {
		t.Parallel()
		a, env := referentAgent(t)
		a.referentMemory = true
		promote(t, env, "x.go")
		for _, sess := range []string{"s4", "s5"} {
			require.NoError(t, env.cmdlog.RecordReferentEpisode(t.Context(), cmdlog.ReferentEpisode{
				Phrase: "other", Target: "y.go", SessionID: sess,
				SourceMessageID: "m-" + sess, Verdict: cmdlog.ReferentAccepted,
			}, params.DefaultMemory().ReferentPromoteHits))
		}
		// #340: only settled labels are evidence — settle the
		// recorded episodes, then fold, so the posterior (not
		// settlement) is the subject under test.
		settleReferents(t, env, env.cmdlog)
		require.NoError(t, env.cmdlog.UpdateProjectRates(t.Context(), params.DefaultMemory().RateDecay))
		refs, gate := a.referentCandidates(t.Context(), SessionAgentCall{
			SessionID: "s6", Prompt: "fix the config",
		}, params.DefaultMemory())
		require.Len(t, refs, 1)
		require.Equal(t, "x.go", refs[0].Target)
		require.NotNil(t, gate)
		require.False(t, gate.gated)
		require.Greater(t, gate.bound, gate.floor)
	})

	t.Run("non-vague prompt fetches nothing", func(t *testing.T) {
		t.Parallel()
		a, env := referentAgent(t)
		a.referentMemory = true
		promote(t, env, "x.go")
		refs, _ := a.referentCandidates(t.Context(), SessionAgentCall{
			SessionID: "s3", Prompt: "refactor internal/config/config.go to split the Options struct",
		}, params.DefaultMemory())
		require.Empty(t, refs)
	})
}

func TestTurnContextSections_ReferentRender(t *testing.T) {
	t.Parallel()
	a, _ := referentAgent(t)
	a.referentMemory = true

	sections := a.turnContextSections(t.Context(), SessionAgentCall{SessionID: "s1"},
		nil, nil, nil, []cmdlog.Referent{
			{Phrase: "config", Target: "internal/config/config.go", Hits: 3},
		}, nil)
	require.Len(t, sections, 1)
	require.Contains(t, sections[0], "<referent_memory>")
	require.Contains(t, sections[0], `"config" usually means`)
	require.Contains(t, sections[0], "internal/config/config.go")
	require.Contains(t, sections[0], "accepted 3")
	require.Contains(t, sections[0], "candidates to verify, not facts")

	// Option off suppresses the section even with candidates.
	a.referentMemory = false
	sections = a.turnContextSections(t.Context(), SessionAgentCall{SessionID: "s1"},
		nil, nil, nil, []cmdlog.Referent{{Phrase: "config", Target: "x.go", Hits: 3}}, nil)
	require.Empty(t, sections)
}

// tailStamp projects the gate decision onto the tail audit (#341):
// nil stays nil (gate never ran — absent stays distinct), the veto
// records rate_below_floor with its bound, an uncomputable bound
// records gate_error with no bound, and a cleared floor admits.
func TestRateGateTailStamp(t *testing.T) {
	t.Parallel()

	var nilGate *rateGateDecision
	require.Nil(t, nilGate.tailStamp())

	admit := (&rateGateDecision{bound: 0.71, floor: 0.6}).tailStamp()
	require.Equal(t, "admit", admit.Reason)
	require.InDelta(t, 0.71, admit.Bound, 1e-9)
	require.InDelta(t, 0.6, admit.Floor, 1e-9)
	require.InDelta(t, 1.0, admit.Propensity, 1e-9)

	veto := (&rateGateDecision{bound: 0.4, floor: 0.6, gated: true}).tailStamp()
	require.Equal(t, "rate_below_floor", veto.Reason)
	require.InDelta(t, 0.4, veto.Bound, 1e-9)

	broken := (&rateGateDecision{floor: 0.6, err: errors.New("bound uncomputable")}).tailStamp()
	require.Equal(t, "gate_error", broken.Reason)
	require.Zero(t, broken.Bound)
	require.InDelta(t, 0.6, broken.Floor, 1e-9)
}
