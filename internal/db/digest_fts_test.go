package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// FTS5 is the session-digest retrieval engine (#164) — this pins that
// the build's SQLite (modernc, CGO disabled) actually supports it.
func TestSessionDigestFTS5Works(t *testing.T) {
	t.Parallel()

	conn, err := Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	q := New(conn)
	ctx := t.Context()

	// The migration created both the digest table and its FTS shadow.
	var name string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE name = 'session_digests_fts'`).Scan(&name))
	require.Equal(t, "session_digests_fts", name)

	require.NoError(t, q.UpsertSessionDigest(ctx, UpsertSessionDigestParams{
		SessionID:    "s1",
		Title:        "login fix",
		Checkpoint:   "fixed the oauth redirect handler",
		Files:        "internal/auth/login.go internal/auth/oauth.go",
		EndedAt:      1000,
		ProjectKey:   "proj",
		ParamVersion: "pv1-x",
	}))
	require.NoError(t, q.IndexSessionDigest(ctx, IndexSessionDigestParams{
		SessionID: "s1",
		Body:      "login fix fixed the oauth redirect handler internal/auth/login.go internal/auth/oauth.go",
	}))

	rows, err := q.SearchSessionDigests(ctx, SearchSessionDigestsParams{
		Body:       "oauth",
		ProjectKey: "proj",
		SessionID:  "current",
		Limit:      5,
	})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "s1", rows[0].SessionID)
	require.Equal(t, "login fix", rows[0].Title)
	require.Equal(t, int64(1000), rows[0].EndedAt)

	// Project partition: the same term under another key finds nothing.
	rows, err = q.SearchSessionDigests(ctx, SearchSessionDigestsParams{
		Body:       "oauth",
		ProjectKey: "other",
		SessionID:  "current",
		Limit:      5,
	})
	require.NoError(t, err)
	require.Empty(t, rows)

	// Index refresh: delete + reinsert replaces the body.
	require.NoError(t, q.DeleteSessionDigestIndex(ctx, "s1"))
	rows, err = q.SearchSessionDigests(ctx, SearchSessionDigestsParams{
		Body:       "oauth",
		ProjectKey: "proj",
		SessionID:  "current",
		Limit:      5,
	})
	require.NoError(t, err)
	require.Empty(t, rows)

	// Digest delete leaves the FTS row removable.
	require.NoError(t, q.DeleteSessionDigest(ctx, "s1"))
}
