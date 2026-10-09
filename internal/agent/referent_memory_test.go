package agent

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/params"
	"github.com/stretchr/testify/require"
)

func referentUserMsg(id, text string) message.Message {
	return message.Message{
		ID:    id,
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: text}},
	}
}

func referentEditMsg(callID, tool, path string) message.Message {
	return message.Message{
		Role: message.Assistant,
		Parts: []message.ContentPart{message.ToolCall{
			ID:       callID,
			Name:     tool,
			Input:    fmt.Sprintf(`{"file_path":%q}`, path),
			Finished: true,
		}},
	}
}

// referentAgent wires a sessionAgent with a real cmdlog service over a
// real db — the end-to-end cases need episode rows to actually land.
func referentAgent(t *testing.T) (*sessionAgent, cmdlog.Service, string) {
	t.Helper()
	env := testEnv(t)
	a := &sessionAgent{
		configStore: config.NewTestStoreWithDir(&config.Config{}, env.workingDir),
		sessions:    env.sessions,
		messages:    env.messages,
		cmdlog:      env.cmdlog,
	}
	return a, env.cmdlog, env.workingDir
}

func TestExtractReferentPhrase(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		prompt string
		want   string
	}{
		{"fix the test", "test"},
		{"update the Config", "config"},
		{"fix the test and the config", ""},            // two referents: unknowable mapping
		{"fix the test twice, the test again", "test"}, // same noun repeated
		{"fix it", ""},                                 // pronoun: no learnable phrase
		{"do the thing", "thing"},
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
		{"revert that", cmdlog.ReferentRevised},
		{"that's not what I meant", cmdlog.ReferentRevised},
		{"oops, undo it", cmdlog.ReferentRevised},
	} {
		t.Run(tc.prompt, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, referentJudgedVerdict(tc.prompt))
		})
	}
}

func TestReferentTargets(t *testing.T) {
	t.Parallel()
	a, _, dir := referentAgent(t)
	abs := filepath.Join(dir, "internal", "cfg.go")

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
	targets := a.referentTargets(msgs, 1)
	require.Len(t, targets, 2)
	require.Equal(t, filepath.Join("internal", "cfg.go"), targets[0].path)
	require.Equal(t, "tc1", targets[0].callID)
	require.Equal(t, "other.go", targets[1].path)
	require.Equal(t, "tc3", targets[1].callID)
}

func TestRecordReferentEpisodes(t *testing.T) {
	t.Parallel()

	t.Run("accepted vague turn records a clean episode", func(t *testing.T) {
		t.Parallel()
		a, svc, dir := referentAgent(t)
		target := filepath.Join(dir, "x.go")
		msgs := []message.Message{
			referentUserMsg("u1", "fix the config"),
			referentEditMsg("tc1", tools.EditToolName, target),
		}
		a.recordReferentEpisodes(t.Context(), SessionAgentCall{
			SessionID: "s1", Prompt: "looks good",
		}, msgs)

		refs, err := svc.ListReferentCandidates(t.Context(), []string{"config"}, 5)
		require.NoError(t, err)
		require.Empty(t, refs) // one clean session is below the default floor

		// A second session re-deriving the same mapping crosses the
		// promotion floor.
		a.recordReferentEpisodes(t.Context(), SessionAgentCall{
			SessionID: "s2", Prompt: "ok",
		}, []message.Message{
			referentUserMsg("u2", "fix the config"),
			referentEditMsg("tc2", tools.EditToolName, target),
		})
		refs, err = svc.ListReferentCandidates(t.Context(), []string{"config"}, 5)
		require.NoError(t, err)
		require.Len(t, refs, 1)
		require.Equal(t, "x.go", refs[0].Target)
	})

	t.Run("revision cue records but does not promote", func(t *testing.T) {
		t.Parallel()
		a, svc, dir := referentAgent(t)
		target := filepath.Join(dir, "x.go")
		a.recordReferentEpisodes(t.Context(), SessionAgentCall{
			SessionID: "s1", Prompt: "wrong file, revert it",
		}, []message.Message{
			referentUserMsg("u1", "fix the config"),
			referentEditMsg("tc1", tools.EditToolName, target),
		})
		a.recordReferentEpisodes(t.Context(), SessionAgentCall{
			SessionID: "s2", Prompt: "fine",
		}, []message.Message{
			referentUserMsg("u2", "fix the config"),
			referentEditMsg("tc2", tools.EditToolName, target),
		})
		// One revised + one accepted: the revised episode still doesn't
		// count, so one clean session stays below the floor.
		refs, err := svc.ListReferentCandidates(t.Context(), []string{"config"}, 5)
		require.NoError(t, err)
		require.Empty(t, refs)
	})

	t.Run("suggested target is contaminated evidence", func(t *testing.T) {
		t.Parallel()
		a, svc, dir := referentAgent(t)
		target := filepath.Join(dir, "x.go")
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
			})
		}
		refs, err := svc.ListReferentCandidates(t.Context(), []string{"config"}, 5)
		require.NoError(t, err)
		require.Empty(t, refs)
	})

	t.Run("explicit-path prompt is not vague and records nothing", func(t *testing.T) {
		t.Parallel()
		a, svc, dir := referentAgent(t)
		target := filepath.Join(dir, "x.go")
		a.recordReferentEpisodes(t.Context(), SessionAgentCall{
			SessionID: "s1", Prompt: "ok",
		}, []message.Message{
			referentUserMsg("u1", "fix "+target),
			referentEditMsg("tc1", tools.EditToolName, target),
		})
		refs, err := svc.ListReferentCandidates(t.Context(), []string{"config"}, 5)
		require.NoError(t, err)
		require.Empty(t, refs)
	})

	t.Run("multi-referent prompt records nothing", func(t *testing.T) {
		t.Parallel()
		a, svc, dir := referentAgent(t)
		target := filepath.Join(dir, "x.go")
		a.recordReferentEpisodes(t.Context(), SessionAgentCall{
			SessionID: "s1", Prompt: "ok",
		}, []message.Message{
			referentUserMsg("u1", "fix the config and the test"),
			referentEditMsg("tc1", tools.EditToolName, target),
		})
		refs, err := svc.ListReferentCandidates(t.Context(), []string{"config", "test"}, 5)
		require.NoError(t, err)
		require.Empty(t, refs)
	})
}

func TestReferentCandidates_Gating(t *testing.T) {
	t.Parallel()

	promote := func(t *testing.T, svc cmdlog.Service, target string) {
		t.Helper()
		for _, sess := range []string{"s1", "s2"} {
			require.NoError(t, svc.RecordReferentEpisode(t.Context(), cmdlog.ReferentEpisode{
				Phrase: "config", Target: target, SessionID: sess,
				SourceMessageID: "m-" + sess, Verdict: cmdlog.ReferentAccepted,
			}, params.DefaultMemory().ReferentPromoteHits))
		}
	}

	t.Run("option off fetches nothing", func(t *testing.T) {
		t.Parallel()
		a, svc, _ := referentAgent(t)
		promote(t, svc, "x.go")
		refs := a.referentCandidates(t.Context(), SessionAgentCall{
			SessionID: "s3", Prompt: "fix the config",
		}, params.DefaultMemory())
		require.Empty(t, refs)
	})

	t.Run("promoted mapping surfaces on a matching vague prompt", func(t *testing.T) {
		t.Parallel()
		a, svc, _ := referentAgent(t)
		a.referentMemory = true
		promote(t, svc, "x.go")
		refs := a.referentCandidates(t.Context(), SessionAgentCall{
			SessionID: "s3", Prompt: "fix the config",
		}, params.DefaultMemory())
		require.Len(t, refs, 1)
		require.Equal(t, "x.go", refs[0].Target)
	})

	t.Run("non-vague prompt fetches nothing", func(t *testing.T) {
		t.Parallel()
		a, svc, _ := referentAgent(t)
		a.referentMemory = true
		promote(t, svc, "x.go")
		refs := a.referentCandidates(t.Context(), SessionAgentCall{
			SessionID: "s3", Prompt: "refactor internal/config/config.go to split the Options struct",
		}, params.DefaultMemory())
		require.Empty(t, refs)
	})
}

func TestTurnContextSections_ReferentRender(t *testing.T) {
	t.Parallel()
	a, _, _ := referentAgent(t)
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
