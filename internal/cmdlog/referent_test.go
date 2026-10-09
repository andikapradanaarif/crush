package cmdlog

import (
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/params"
	"github.com/stretchr/testify/require"
)

func referentEp(session, msg, target, verdict string, suggested bool) ReferentEpisode {
	return ReferentEpisode{
		Phrase:          "config",
		Target:          target,
		SessionID:       session,
		SourceMessageID: msg,
		ToolCallID:      "call-" + msg,
		RepoState:       "abc123",
		Suggested:       suggested,
		Verdict:         verdict,
	}
}

func TestRecordReferentEpisode_PromotesOnDistinctSessions(t *testing.T) {
	env := setupTest(t)

	require.NoError(t, env.svc.RecordReferentEpisode(env.ctx,
		referentEp("s1", "m1", "internal/config/config.go", ReferentAccepted, false), 2))
	// One clean session is below the floor — nothing to render yet.
	refs, err := env.svc.ListReferentCandidates(env.ctx, []string{"config"}, 5)
	require.NoError(t, err)
	require.Empty(t, refs)

	require.NoError(t, env.svc.RecordReferentEpisode(env.ctx,
		referentEp("s2", "m2", "internal/config/config.go", ReferentAccepted, false), 2))

	refs, err = env.svc.ListReferentCandidates(env.ctx, []string{"config"}, 5)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.Equal(t, "config", refs[0].Phrase)
	require.Equal(t, "internal/config/config.go", refs[0].Target)
	require.EqualValues(t, 1, refs[0].Hits)
}

func TestRecordReferentEpisode_SameSessionDoesNotPromote(t *testing.T) {
	env := setupTest(t)

	// Two episodes, same session, different vague messages — distinct-
	// session counting means this is one observation repeated.
	require.NoError(t, env.svc.RecordReferentEpisode(env.ctx,
		referentEp("s1", "m1", "x.go", ReferentAccepted, false), 2))
	require.NoError(t, env.svc.RecordReferentEpisode(env.ctx,
		referentEp("s1", "m2", "x.go", ReferentAccepted, false), 2))

	refs, err := env.svc.ListReferentCandidates(env.ctx, []string{"config"}, 5)
	require.NoError(t, err)
	require.Empty(t, refs)
}

func TestRecordReferentEpisode_SuggestedDoesNotPromote(t *testing.T) {
	env := setupTest(t)

	// Memory-suggested targets are echo, not evidence — two suggested
	// acceptances from distinct sessions must not promote.
	require.NoError(t, env.svc.RecordReferentEpisode(env.ctx,
		referentEp("s1", "m1", "x.go", ReferentAccepted, true), 2))
	require.NoError(t, env.svc.RecordReferentEpisode(env.ctx,
		referentEp("s2", "m2", "x.go", ReferentAccepted, true), 2))

	refs, err := env.svc.ListReferentCandidates(env.ctx, []string{"config"}, 5)
	require.NoError(t, err)
	require.Empty(t, refs)
}

func TestRecordReferentEpisode_RevisedDoesNotPromote(t *testing.T) {
	env := setupTest(t)

	require.NoError(t, env.svc.RecordReferentEpisode(env.ctx,
		referentEp("s1", "m1", "x.go", ReferentRevised, false), 1))

	refs, err := env.svc.ListReferentCandidates(env.ctx, []string{"config"}, 5)
	require.NoError(t, err)
	require.Empty(t, refs)
}

func TestRecordReferentEpisode_RetryIsIdempotent(t *testing.T) {
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	svc := NewService(db.New(conn), t.TempDir(), params.DefaultMemory())
	ctx := t.Context()

	ep := referentEp("s1", "m1", "x.go", ReferentAccepted, false)
	require.NoError(t, svc.RecordReferentEpisode(ctx, ep, 2))
	// A repair-chain re-run re-derives the same episode — the
	// (session, source_message, target) unique key must absorb it.
	require.NoError(t, svc.RecordReferentEpisode(ctx, ep, 2))

	var count int
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM referent_episodes`).Scan(&count))
	require.Equal(t, 1, count)

	// The duplicate cannot double-count toward promotion either.
	refs, err := svc.ListReferentCandidates(ctx, []string{"config"}, 5)
	require.NoError(t, err)
	require.Empty(t, refs)
}

func TestRecordReferentEpisode_ProjectPartition(t *testing.T) {
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	q := db.New(conn)
	dir := t.TempDir()

	svcA := NewService(q, dir, params.DefaultMemory(), WithProjectKey("proj-a"))
	svcB := NewService(q, dir, params.DefaultMemory(), WithProjectKey("proj-b"))

	require.NoError(t, svcA.RecordReferentEpisode(t.Context(),
		referentEp("s1", "m1", "x.go", ReferentAccepted, false), 1))

	// The mapping promoted under proj-a — proj-b sees nothing.
	refsA, err := svcA.ListReferentCandidates(t.Context(), []string{"config"}, 5)
	require.NoError(t, err)
	require.Len(t, refsA, 1)
	require.Equal(t, "proj-a", refsA[0].ProjectKey)

	refsB, err := svcB.ListReferentCandidates(t.Context(), []string{"config"}, 5)
	require.NoError(t, err)
	require.Empty(t, refsB)
}

func TestListReferentCandidates_LimitAndOrder(t *testing.T) {
	env := setupTest(t)

	// Promote two targets under different phrases with different mass.
	for _, tc := range []struct {
		phrase  string
		target  string
		session string
	}{
		{"config", "a.go", "s1"},
		{"test", "b_test.go", "s2"},
		{"test", "b_test.go", "s3"},
	} {
		ep := referentEp(tc.session, "m-"+tc.session, tc.target, ReferentAccepted, false)
		ep.Phrase = tc.phrase
		require.NoError(t, env.svc.RecordReferentEpisode(env.ctx, ep, 1))
	}

	refs, err := env.svc.ListReferentCandidates(env.ctx, []string{"config", "test"}, 5)
	require.NoError(t, err)
	require.Len(t, refs, 2)
	// "test" has two promotes (hits=2), "config" one — hits DESC.
	require.Equal(t, "test", refs[0].Phrase)
	require.Equal(t, "config", refs[1].Phrase)

	refs, err = env.svc.ListReferentCandidates(env.ctx, []string{"config", "test"}, 1)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.Equal(t, "test", refs[0].Phrase)
}

func TestMarkSuggestedFile_RoundTrip(t *testing.T) {
	env := setupTest(t)

	require.False(t, env.svc.WasSuggestedFile(env.ctx, "s1", "x.go"))
	env.svc.MarkSuggestedFile("s1", "x.go")
	require.True(t, env.svc.WasSuggestedFile(env.ctx, "s1", "x.go"))
	// The file keyspace is separate from the command keyspace.
	require.False(t, env.svc.WasSuggestedFile(env.ctx, "s1", "y.go"))
	require.False(t, env.svc.WasSuggestedFile(env.ctx, "s2", "x.go"))
}

func TestRecordReferentEpisode_ZeroPromoteMinNeverPromotes(t *testing.T) {
	env := setupTest(t)

	require.NoError(t, env.svc.RecordReferentEpisode(env.ctx,
		referentEp("s1", "m1", "x.go", ReferentAccepted, false), 0))

	refs, err := env.svc.ListReferentCandidates(env.ctx, []string{"config"}, 5)
	require.NoError(t, err)
	require.Empty(t, refs)
}
