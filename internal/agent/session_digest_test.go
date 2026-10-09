package agent

import (
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/params"
	"github.com/stretchr/testify/require"
)

// digestAgent wires a sessionAgent with the real cmdlog service over a
// real db — the digest path needs actual session rows to refresh from.
func digestAgent(t *testing.T) (*sessionAgent, fakeEnv) {
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

func TestContinuationCue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		prompt string
		want   bool
	}{
		{"continue", true},
		{"pick up where we left off", true},
		{"what did we do yesterday", true},
		{"resume the login work", true},
		{"what were we doing last session", true},
		{"refactor internal/config/config.go", false},
		{"add a README", false},
		// "continue working on x" is still a continuation cue.
		{"continue working on auth", true},
	} {
		t.Run(tc.prompt, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, continuationCueRe.MatchString(tc.prompt))
		})
	}
}

func TestSessionReferent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		prompt string
		want   bool
	}{
		{"the login thing", true},
		{"the oauth work", true},
		{"that session", true},
		{"that auth session", true},
		{"the stuff we did", true},
		// Anchored objects stay anchored — no recall fetch.
		{"the login page", false},
		{"the config file", false},
		{"refactor internal/auth/login.go", false},
		{"fix the flaky test", false},
	} {
		t.Run(tc.prompt, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, sessionReferentRe.MatchString(tc.prompt))
		})
	}
}

func TestDigestCandidates_Gating(t *testing.T) {
	t.Parallel()
	mp := params.DefaultMemory()

	seedPrior := func(t *testing.T, env fakeEnv) {
		t.Helper()
		// A prior session worth digesting: title + a touched path.
		_, err := env.sessions.Create(t.Context(), "fix login redirect")
		require.NoError(t, err)
		require.NoError(t, env.cmdlog.RefreshSessionDigests(t.Context(), mp.DigestRefreshLimit))
	}

	t.Run("option off fetches nothing", func(t *testing.T) {
		t.Parallel()
		a, env := digestAgent(t)
		seedPrior(t, env)
		got := a.digestCandidates(t.Context(), SessionAgentCall{
			SessionID: "cur", Prompt: "the login thing",
		}, mp)
		require.Empty(t, got)
	})

	t.Run("vague prompt surfaces a matching digest", func(t *testing.T) {
		t.Parallel()
		a, env := digestAgent(t)
		a.sessionMemory = true
		seedPrior(t, env)
		got := a.digestCandidates(t.Context(), SessionAgentCall{
			SessionID: "cur", Prompt: "the login thing",
		}, mp)
		require.Len(t, got, 1)
		require.Equal(t, "fix login redirect", got[0].Title)
	})

	t.Run("continuation falls back to recency", func(t *testing.T) {
		t.Parallel()
		a, env := digestAgent(t)
		a.sessionMemory = true
		seedPrior(t, env)
		got := a.digestCandidates(t.Context(), SessionAgentCall{
			SessionID: "cur", Prompt: "continue",
		}, mp)
		require.Len(t, got, 1)
		require.Equal(t, "fix login redirect", got[0].Title)
	})

	t.Run("non-vague non-continuation fetches nothing", func(t *testing.T) {
		t.Parallel()
		a, env := digestAgent(t)
		a.sessionMemory = true
		seedPrior(t, env)
		got := a.digestCandidates(t.Context(), SessionAgentCall{
			SessionID: "cur",
			Prompt:    "refactor internal/auth/login.go to split the handler chain",
		}, mp)
		require.Empty(t, got)
	})

	t.Run("sub-agent fetches nothing", func(t *testing.T) {
		t.Parallel()
		a, env := digestAgent(t)
		a.sessionMemory = true
		a.isSubAgent = true
		seedPrior(t, env)
		got := a.digestCandidates(t.Context(), SessionAgentCall{
			SessionID: "cur", Prompt: "the login thing",
		}, mp)
		require.Empty(t, got)
	})

	t.Run("repair attempt fetches nothing", func(t *testing.T) {
		t.Parallel()
		a, env := digestAgent(t)
		a.sessionMemory = true
		seedPrior(t, env)
		got := a.digestCandidates(t.Context(), SessionAgentCall{
			SessionID: "cur", Prompt: "the login thing", RepairAttempts: 1,
		}, mp)
		require.Empty(t, got)
	})
}

func TestDigestTail_MarksSuggestedFiles(t *testing.T) {
	t.Parallel()
	a, env := digestAgent(t)
	a.sessionMemory = true

	// A prior session that read a file — the rendered digest's file
	// hints mark the current session so a later edit flags as
	// memory-informed, not independent evidence.
	sess, err := env.sessions.Create(t.Context(), "fix login redirect")
	require.NoError(t, err)
	(*env.filetracker).RecordRead(t.Context(), sess.ID, "internal/auth/login.go")

	cur, err := env.sessions.Create(t.Context(), "current session")
	require.NoError(t, err)

	tail := a.turnTailMessages(t.Context(), SessionAgentCall{
		SessionID: cur.ID, Prompt: "the login thing",
	}, nil)
	require.Len(t, tail, 1)
	require.True(t, env.cmdlog.WasSuggestedFile(t.Context(), cur.ID, "internal/auth/login.go"))
	require.False(t, env.cmdlog.WasSuggestedFile(t.Context(), cur.ID, "internal/auth/oauth.go"))
}

func TestDigestTail_HoldoutSuppressesAndNeverMarks(t *testing.T) {
	t.Parallel()
	a, env := digestAgent(t)
	a.sessionMemory = true

	// A digestible prior session exists, so the difference is purely
	// the holdout arm, not the fetch.
	sess, err := env.sessions.Create(t.Context(), "fix login redirect")
	require.NoError(t, err)
	(*env.filetracker).RecordRead(t.Context(), sess.ID, "internal/auth/login.go")

	// Force the holdout coin — the arm suppresses the render and the
	// marks alike: a suppressed turn must not leak through the
	// suggested-file channel either.
	a.memoryTelemetry = newMemoryTelemetry(true, t.TempDir(), env.workingDir, "pk", "pv")
	a.memoryTelemetry.roll = func(string) float64 { return 0.05 }

	cur, err := env.sessions.Create(t.Context(), "current session")
	require.NoError(t, err)
	tail := a.turnTailMessages(t.Context(), SessionAgentCall{
		SessionID: cur.ID, Prompt: "the login thing",
	}, nil)
	require.Empty(t, tail)
	require.False(t, env.cmdlog.WasSuggestedFile(t.Context(), cur.ID, "internal/auth/login.go"))
}

func TestTurnContextSections_SessionMemoryRender(t *testing.T) {
	t.Parallel()
	a, _ := digestAgent(t)
	a.sessionMemory = true

	digests := []cmdlog.SessionDigest{
		{
			SessionID: "prior-1",
			Title:     "fix login redirect",
			EndedAt:   time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC),
			Files:     []string{"internal/auth/login.go", "internal/auth/oauth.go"},
		},
		{
			SessionID: "prior-2",
			EndedAt:   time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC),
		},
	}

	sections := a.turnContextSections(t.Context(), SessionAgentCall{SessionID: "s1"},
		nil, nil, nil, nil, digests)
	require.Len(t, sections, 1)
	require.Contains(t, sections[0], "<session_memory>")
	require.Contains(t, sections[0], `"fix login redirect" (2026-10-09)`)
	require.Contains(t, sections[0], "internal/auth/login.go, internal/auth/oauth.go")
	require.Contains(t, sections[0], "(untitled session)")
	require.Contains(t, sections[0], "pointers (title, date, files) to reopen, not facts")
	require.Contains(t, sections[0], "</session_memory>")

	// File hints bound at DigestFileHints.
	many := []cmdlog.SessionDigest{{
		SessionID: "prior-3", Title: "wide", EndedAt: time.Now(),
		Files: []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go", "g.go", "h.go"},
	}}
	sections = a.turnContextSections(t.Context(), SessionAgentCall{SessionID: "s1"},
		nil, nil, nil, nil, many)
	require.Contains(t, sections[0], "f.go")
	require.NotContains(t, sections[0], "g.go")

	// Option off suppresses the section even with candidates.
	a.sessionMemory = false
	sections = a.turnContextSections(t.Context(), SessionAgentCall{SessionID: "s1"},
		nil, nil, nil, nil, digests)
	require.Empty(t, sections)
}
