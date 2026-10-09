package cmdlog

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/notebook"
	"github.com/charmbracelet/crush/internal/params"
	"github.com/stretchr/testify/require"
)

// digestEnv bundles a digest-writing service with the raw conn for
// fixture SQL the generated queries don't cover (timestamp overrides).
type digestEnv struct {
	ctx  context.Context
	svc  Service
	q    *db.Queries
	conn *sql.DB
}

func setupDigestTest(t *testing.T) *digestEnv {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	q := db.New(conn)
	return &digestEnv{
		ctx:  t.Context(),
		svc:  NewService(q, t.TempDir(), params.DefaultMemory()),
		q:    q,
		conn: conn,
	}
}

// seedSession inserts a top-level session with a pinned updated_at.
// INSERT keeps the value — the AFTER UPDATE trigger re-stamps any
// UPDATE to strftime('now'), so pinning requires inserting directly.
func seedSession(t *testing.T, env *digestEnv, id, title string, updatedAt int64) {
	t.Helper()
	_, err := env.conn.ExecContext(env.ctx,
		`INSERT INTO sessions (id, parent_session_id, title, message_count, updated_at, created_at)
		 VALUES (?, NULL, ?, 2, ?, ?)`, id, title, updatedAt, updatedAt)
	require.NoError(t, err)
}

func seedRead(t *testing.T, env *digestEnv, sessionID, path string) {
	t.Helper()
	require.NoError(t, env.q.RecordFileRead(env.ctx, db.RecordFileReadParams{
		SessionID: sessionID,
		Path:      path,
	}))
}

func seedCheckpoint(t *testing.T, env *digestEnv, sessionID string, turn int64, title, text string) {
	t.Helper()
	_, err := env.q.CreateNotebookEntry(env.ctx, db.CreateNotebookEntryParams{
		ID:          fmt.Sprintf("%s-ck-%d", sessionID, turn),
		SessionID:   sessionID,
		TurnNumber:  turn,
		EventNumber: 1,
		EventType:   notebook.EventCheckpoint,
		Title:       title,
		EntryText:   text,
	})
	require.NoError(t, err)
}

func TestRefreshSessionDigests_BuildsDigest(t *testing.T) {
	t.Parallel()
	env := setupDigestTest(t)
	seedSession(t, env, "s1", "fix the login redirect", 1000)
	seedRead(t, env, "s1", "internal/auth/login.go")
	seedRead(t, env, "s1", "internal/auth/oauth.go")
	seedCheckpoint(t, env, "s1", 3, "Checkpoint", "fixed the oauth redirect handler")

	require.NoError(t, env.svc.RefreshSessionDigests(env.ctx, 10))

	d, err := env.q.GetSessionDigest(env.ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, "fix the login redirect", d.Title)
	require.Contains(t, d.Checkpoint, "oauth redirect")
	require.Contains(t, d.Files, "internal/auth/login.go")
	require.Equal(t, int64(1000), d.EndedAt)
	require.Equal(t, env.svc.ProjectKey(), d.ProjectKey)
	require.Equal(t, env.svc.ParamVersion(), d.ParamVersion)

	// The FTS shadow row exists for the same session.
	var body string
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT body FROM session_digests_fts WHERE session_id = 's1'`).Scan(&body))
	require.Contains(t, body, "oauth")
	require.Contains(t, body, "login.go")
}

func TestRefreshSessionDigests_LatestCheckpointWins(t *testing.T) {
	t.Parallel()
	env := setupDigestTest(t)
	seedSession(t, env, "s1", "work", 500)
	seedCheckpoint(t, env, "s1", 1, "Checkpoint", "early state: exploring handlers")
	seedCheckpoint(t, env, "s1", 4, "Checkpoint", "late state: migrated to jwt")

	require.NoError(t, env.svc.RefreshSessionDigests(env.ctx, 10))
	d, err := env.q.GetSessionDigest(env.ctx, "s1")
	require.NoError(t, err)
	require.Contains(t, d.Checkpoint, "jwt")
	require.NotContains(t, d.Checkpoint, "exploring")
}

func TestRefreshSessionDigests_TitleFilesFallback(t *testing.T) {
	t.Parallel()
	env := setupDigestTest(t)
	seedSession(t, env, "s1", "tinker with css", 300)
	seedRead(t, env, "s1", "web/app.css")

	require.NoError(t, env.svc.RefreshSessionDigests(env.ctx, 10))
	d, err := env.q.GetSessionDigest(env.ctx, "s1")
	require.NoError(t, err)
	require.Empty(t, d.Checkpoint)
	require.Contains(t, d.Files, "web/app.css")
}

func TestRefreshSessionDigests_SkipsFreshRows(t *testing.T) {
	t.Parallel()
	env := setupDigestTest(t)
	seedSession(t, env, "s1", "original title", 1000)
	require.NoError(t, env.svc.RefreshSessionDigests(env.ctx, 10))

	// Fresh digest: same updated_at → the stale query skips it.
	require.NoError(t, env.svc.RefreshSessionDigests(env.ctx, 10))

	// Any UPDATE re-stamps updated_at to strftime('now') via trigger —
	// that's the staleness signal the refresh keys on.
	_, err := env.conn.ExecContext(env.ctx,
		`UPDATE sessions SET title = 'renamed work' WHERE id = 's1'`)
	require.NoError(t, err)
	seedCheckpoint(t, env, "s1", 9, "Checkpoint", "landed after rename")
	require.NoError(t, env.svc.RefreshSessionDigests(env.ctx, 10))
	d, err := env.q.GetSessionDigest(env.ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, "renamed work", d.Title)
	require.Greater(t, d.EndedAt, int64(1000))
	require.Contains(t, d.Checkpoint, "landed after rename")
}

func TestRefreshSessionDigests_SkipsSubagentSessions(t *testing.T) {
	t.Parallel()
	env := setupDigestTest(t)
	seedSession(t, env, "s1", "parent work", 1000)
	// Task sessions carry parent_session_id — internal machinery, not
	// user work, so they never earn a digest.
	_, err := env.q.CreateSession(env.ctx, db.CreateSessionParams{
		ID:              "task-1",
		ParentSessionID: sql.NullString{String: "s1", Valid: true},
		Title:           "subagent task",
	})
	require.NoError(t, err)

	require.NoError(t, env.svc.RefreshSessionDigests(env.ctx, 10))
	_, err = env.q.GetSessionDigest(env.ctx, "task-1")
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestRefreshSessionDigests_ReadWriteUnion(t *testing.T) {
	t.Parallel()
	env := setupDigestTest(t)
	seedSession(t, env, "s1", "edit the config", 700)
	seedRead(t, env, "s1", "internal/config/load.go")
	// Writes land in files; reads land in read_files — the digest's
	// touched set is the union of both.
	_, err := env.q.CreateFile(env.ctx, db.CreateFileParams{
		ID:        "f1",
		SessionID: "s1",
		Path:      "internal/config/config.go",
		Content:   "package config",
		Version:   1,
	})
	require.NoError(t, err)

	require.NoError(t, env.svc.RefreshSessionDigests(env.ctx, 10))
	d, err := env.q.GetSessionDigest(env.ctx, "s1")
	require.NoError(t, err)
	require.Contains(t, d.Files, "internal/config/load.go")
	require.Contains(t, d.Files, "internal/config/config.go")
}

func TestRefreshSessionDigests_LimitBounds(t *testing.T) {
	t.Parallel()
	env := setupDigestTest(t)
	seedSession(t, env, "s1", "first", 1000)
	seedSession(t, env, "s2", "second", 2000)
	seedSession(t, env, "s3", "third", 3000)

	// The bound amortizes refresh across calls — the freshest go first.
	require.NoError(t, env.svc.RefreshSessionDigests(env.ctx, 2))
	var n int
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT COUNT(*) FROM session_digests`).Scan(&n))
	require.Equal(t, 2, n)
	_, err := env.q.GetSessionDigest(env.ctx, "s3")
	require.NoError(t, err)
	_, err = env.q.GetSessionDigest(env.ctx, "s1")
	require.ErrorIs(t, err, sql.ErrNoRows)

	require.NoError(t, env.svc.RefreshSessionDigests(env.ctx, 2))
	_, err = env.q.GetSessionDigest(env.ctx, "s1")
	require.NoError(t, err)
}

func TestSearchSessionDigests_ParaphraseMatch(t *testing.T) {
	t.Parallel()
	env := setupDigestTest(t)
	seedSession(t, env, "s1", "fix login redirect", 1000)
	seedRead(t, env, "s1", "internal/auth/login.go")
	seedCheckpoint(t, env, "s1", 2, "Checkpoint", "OAuth callback loop resolved")
	seedSession(t, env, "s2", "tweak table styles", 2000)
	seedRead(t, env, "s2", "web/table.css")
	require.NoError(t, env.svc.RefreshSessionDigests(env.ctx, 10))

	// "the login thing" paraphrases the stored title/checkpoint.
	rows, err := env.svc.SearchSessionDigests(env.ctx, "the login thing", "current", 5)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "s1", rows[0].SessionID)
	require.Equal(t, "fix login redirect", rows[0].Title)
	// Search rows carry the provenance pack through, same as the
	// recency path — the pointer is attributable either way.
	require.Equal(t, env.svc.ProjectKey(), rows[0].ProjectKey)
	require.Equal(t, env.svc.ParamVersion(), rows[0].ParamVersion)

	// A term nobody indexed finds nothing — no fallback here; the
	// recency path is the caller's continuation-cue branch.
	rows, err = env.svc.SearchSessionDigests(env.ctx, "kubernetes migration", "current", 5)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestSearchSessionDigests_ExcludesCurrentSession(t *testing.T) {
	t.Parallel()
	env := setupDigestTest(t)
	seedSession(t, env, "s1", "login work", 1000)
	seedRead(t, env, "s1", "internal/auth/login.go")
	require.NoError(t, env.svc.RefreshSessionDigests(env.ctx, 10))

	rows, err := env.svc.SearchSessionDigests(env.ctx, "login", "s1", 5)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestSearchSessionDigests_ProjectPartition(t *testing.T) {
	t.Parallel()
	env := setupDigestTest(t)
	seedSession(t, env, "s1", "login work", 1000)
	seedRead(t, env, "s1", "internal/auth/login.go")
	require.NoError(t, env.svc.RefreshSessionDigests(env.ctx, 10))

	// A second service over the same db rooted elsewhere owns a
	// different project_key — its searches must not see our digests.
	other := NewService(env.q, t.TempDir(), params.DefaultMemory())
	require.NotEqual(t, env.svc.ProjectKey(), other.ProjectKey())
	rows, err := other.SearchSessionDigests(env.ctx, "login", "current", 5)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestRecentSessionDigests_RecencyAndExclusion(t *testing.T) {
	t.Parallel()
	env := setupDigestTest(t)
	seedSession(t, env, "s1", "older work", 1000)
	seedSession(t, env, "s2", "newer work", 2000)
	seedSession(t, env, "s3", "current session", 3000)
	require.NoError(t, env.svc.RefreshSessionDigests(env.ctx, 10))

	rows, err := env.svc.RecentSessionDigests(env.ctx, "s3", 5)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "s2", rows[0].SessionID)
	require.Equal(t, "s1", rows[1].SessionID)
}

func TestDeleteSessionDigest_RemovesBothRows(t *testing.T) {
	t.Parallel()
	env := setupDigestTest(t)
	seedSession(t, env, "s1", "login work", 1000)
	seedRead(t, env, "s1", "internal/auth/login.go")
	require.NoError(t, env.svc.RefreshSessionDigests(env.ctx, 10))

	require.NoError(t, env.svc.DeleteSessionDigest(env.ctx, "s1"))
	_, err := env.q.GetSessionDigest(env.ctx, "s1")
	require.ErrorIs(t, err, sql.ErrNoRows)
	var n int
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT COUNT(*) FROM session_digests_fts WHERE session_id = 's1'`).Scan(&n))
	require.Zero(t, n)
}

func TestDigestTerms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		prompt string
		want   string
	}{
		{"the login thing", `"login"`},
		{"continue", ""},
		{"what did we do yesterday", ""},
		{"fix the broken test", `"fix" OR "broken" OR "test"`},
		{"that session where we set up oauth; it broke", `"set" OR "oauth" OR "broke"`},
	}
	for _, tc := range cases {
		t.Run(tc.prompt, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, digestTerms(tc.prompt))
		})
	}
}

func TestDigestTerms_QuotedAndCapped(t *testing.T) {
	t.Parallel()
	// FTS5 syntax chars in the prompt get quoted inside terms, never
	// interpreted.
	q := digestTerms(`drop table sessions; "evil" OR *`)
	require.NotContains(t, q, "*")
	require.NotContains(t, q, "drop table")

	// More than the term cap truncates.
	q = digestTerms("alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo")
	require.Equal(t, digestQueryMaxTerms, strings.Count(q, `"`)/2)
}
