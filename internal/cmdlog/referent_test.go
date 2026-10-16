package cmdlog

import (
	"database/sql"
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

// seedSettledReferent inserts one referent_episodes row already
// frozen as a clean success — the shape promotion counts, since
// #340 gates evidence on settled outcomes rather than raw cues.
func seedSettledReferent(t *testing.T, conn *sql.DB, projectKey, session, msg, phrase, target string) {
	t.Helper()
	_, err := conn.ExecContext(t.Context(),
		`INSERT INTO referent_episodes
		 (phrase, target, session_id, source_message_id, tool_call_id,
		  repo_state, memory_suggested, verdict, project_key,
		  param_version, created_at, label_committed, label_hash_changed,
		  settled_at)
		 VALUES (?, ?, ?, ?, 'c', '', 0, 'accepted', ?, '', 1, 1, NULL, 1)`,
		phrase, target, session, msg, projectKey)
	require.NoError(t, err)
}

func TestRecordReferentEpisode_PromotesOnDistinctSessions(t *testing.T) {
	env := setupTest(t)

	// A fresh episode is unsettled and cannot count itself: one
	// frozen acceptance stays below the floor when observed.
	seedSettledReferent(t, env.conn, env.svc.(*service).projectKey, "s1", "m1", "config", "internal/config/config.go")
	require.NoError(t, env.svc.RecordReferentEpisode(env.ctx,
		referentEp("s2", "m2", "internal/config/config.go", ReferentAccepted, false), 2))
	refs, err := env.svc.ListReferentCandidates(env.ctx, []string{"config"}, 5)
	require.NoError(t, err)
	require.Empty(t, refs)

	// A second frozen session's evidence crosses the floor — the
	// record path sees it and promotes immediately.
	seedSettledReferent(t, env.conn, env.svc.(*service).projectKey, "s3", "m3", "config", "internal/config/config.go")
	require.NoError(t, env.svc.RecordReferentEpisode(env.ctx,
		referentEp("s4", "m4", "internal/config/config.go", ReferentAccepted, false), 2))

	refs, err = env.svc.ListReferentCandidates(env.ctx, []string{"config"}, 5)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.Equal(t, "config", refs[0].Phrase)
	require.Equal(t, "internal/config/config.go", refs[0].Target)
	// The row is born with the full clean-acceptance count that
	// crossed the floor — two settled sessions promoted it.
	require.EqualValues(t, 2, refs[0].Hits)
}

// The crossing observation can also be the settling row itself: an
// episode whose success freezes at settlement promotes the mapping
// even though the record path could not count it.
func TestSettleReferentLabels_PromotesOnFrozenOutcome(t *testing.T) {
	env := setupTest(t)

	seedSettledReferent(t, env.conn, env.svc.(*service).projectKey, "s1", "m1", "config", "internal/config/config.go")
	require.NoError(t, env.svc.RecordReferentEpisode(env.ctx,
		referentEp("s2", "m2", "internal/config/config.go", ReferentAccepted, false), 2))

	// Not yet: the new row is unsettled, so evidence sits at 1.
	refs, err := env.svc.ListReferentCandidates(env.ctx, []string{"config"}, 5)
	require.NoError(t, err)
	require.Empty(t, refs)

	// Force the window closed — settle_at stamps +24h at insert now;
	// with no git or fingerprints the settled outcome falls back to
	// the accepted cue.
	_, err = env.conn.ExecContext(env.ctx,
		`UPDATE referent_episodes SET settle_at = 0 WHERE settled_at IS NULL`)
	require.NoError(t, err)
	require.NoError(t, env.svc.SettleReferentLabels(env.ctx, 50))

	refs, err = env.svc.ListReferentCandidates(env.ctx, []string{"config"}, 5)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.EqualValues(t, 2, refs[0].Hits)
}

// A settled failure outcome cannot promote — the frozen revised cue
// (or diverged artifact) is evidence against, not for.
func TestSettleReferentLabels_FailedOutcomeDoesNotPromote(t *testing.T) {
	env := setupTest(t)

	seedSettledReferent(t, env.conn, env.svc.(*service).projectKey, "s1", "m1", "config", "x.go")
	require.NoError(t, env.svc.RecordReferentEpisode(env.ctx,
		referentEp("s2", "m2", "x.go", ReferentRevised, false), 2))

	_, err := env.conn.ExecContext(env.ctx,
		`UPDATE referent_episodes SET settle_at = 0 WHERE settled_at IS NULL`)
	require.NoError(t, err)
	require.NoError(t, env.svc.SettleReferentLabels(env.ctx, 50))

	refs, err := env.svc.ListReferentCandidates(env.ctx, []string{"config"}, 5)
	require.NoError(t, err)
	require.Empty(t, refs)
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

// An unclassifiable follow-up records the episode — the observation
// happened and later artifact-based labeling can revisit it — but
// unknown is not evidence: it must never count toward promotion.
func TestRecordReferentEpisode_UnknownRecordsNotPromotes(t *testing.T) {
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	svc := NewService(db.New(conn), t.TempDir(), params.DefaultMemory())
	ctx := t.Context()

	require.NoError(t, svc.RecordReferentEpisode(ctx,
		referentEp("s1", "m1", "x.go", ReferentUnknown, false), 1))
	require.NoError(t, svc.RecordReferentEpisode(ctx,
		referentEp("s2", "m2", "x.go", ReferentUnknown, false), 1))

	refs, err := svc.ListReferentCandidates(ctx, []string{"config"}, 5)
	require.NoError(t, err)
	require.Empty(t, refs)

	var count int
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM referent_episodes WHERE verdict = 'unknown'`).Scan(&count))
	require.Equal(t, 2, count)
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

	// One frozen clean acceptance under proj-a; the record observes
	// it and promotes (floor 1) inside that partition only.
	seedSettledReferent(t, conn, "proj-a", "s1", "m1", "config", "x.go")
	require.NoError(t, svcA.RecordReferentEpisode(t.Context(),
		referentEp("s2", "m2", "x.go", ReferentAccepted, false), 1))

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

	// Promote two targets under different phrases with different mass —
	// settled evidence only, so seeds carry the frozen-success shape.
	for _, tc := range []struct {
		phrase  string
		target  string
		session string
	}{
		{"config", "a.go", "s1"},
		{"test", "b_test.go", "s2"},
		{"test", "b_test.go", "s3"},
	} {
		seedSettledReferent(t, env.conn, env.svc.(*service).projectKey,
			tc.session, "m-"+tc.session, tc.phrase, tc.target)
		ep := referentEp(tc.session+"-live", "m-"+tc.session+"-live", tc.target, ReferentAccepted, false)
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
