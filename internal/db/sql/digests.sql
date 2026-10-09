-- Session digests (#164): one resumable row per session + an FTS5
-- shadow for paraphrased cross-session retrieval. Pointers only ---
-- titles, paths, dates.
--
-- FTS5 notes: sqlc catalogs the virtual table's columns but cannot
-- resolve `session_digests_fts MATCH` (table-name MATCH), so queries
-- use `body MATCH` --- equivalent here since body is the only indexed
-- column. Explicit column lists everywhere: sqlc's `*` expansion
-- re-parses badly against this schema.

-- name: GetLatestCheckpointEntry :one
-- The session's consolidated position --- the digest's narrative
-- field where the notebook produced one. Coarsest granularity wins
-- before recency: a session- or boundary-grain checkpoint carries the
-- whole position, so a turn-grain one written later must not displace
-- it. Untagged checkpoints rank coarsest, matching granularityRank.
SELECT e.title, e.entry_text FROM notebook_entries e
LEFT JOIN notebook_tags t ON t.entry_id = e.id
    AND t.tag LIKE 'granularity:%'
WHERE e.session_id = ? AND e.event_type = 'checkpoint'
ORDER BY
    CASE t.tag
        WHEN 'granularity:turn' THEN 0
        WHEN 'granularity:boundary' THEN 1
        WHEN 'granularity:session' THEN 2
        ELSE 3
    END DESC,
    e.turn_number DESC,
    e.event_number DESC
LIMIT 1;

-- name: ListStaleDigestSessions :many
-- Sessions whose digest is missing or older than their last update ---
-- the lazy-refresh worklist. Sub-agent sessions (parent_session_id
-- set) are internal machinery, not user work, so they stay out.
SELECT s.id, s.title, s.updated_at
FROM sessions s
LEFT JOIN session_digests d ON d.session_id = s.id
WHERE s.parent_session_id IS NULL
  AND (d.session_id IS NULL OR s.updated_at > d.ended_at)
ORDER BY s.updated_at DESC
LIMIT ?;

-- name: ListSessionTouchedPaths :many
-- Read files are already workspace-relative; files rows store the
-- path the tool passed, relativized service-side when needed.
SELECT DISTINCT path FROM read_files WHERE read_files.session_id = ?
UNION
SELECT DISTINCT path FROM files WHERE files.session_id = ?
ORDER BY path;

-- name: UpsertSessionDigest :exec
-- The digest is a per-turn refresh of the session's resumable view:
-- title + latest checkpoint + touched files. ended_at follows the
-- session row's updated_at --- the closest thing to an end timestamp a
-- session has (sessions never formally end).
INSERT INTO session_digests (
    session_id, title, checkpoint, files, ended_at, project_key,
    param_version
) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (session_id) DO UPDATE SET
    title = excluded.title,
    checkpoint = excluded.checkpoint,
    files = excluded.files,
    ended_at = excluded.ended_at,
    project_key = excluded.project_key,
    param_version = excluded.param_version;

-- name: ListRecentSessionDigests :many
-- The recency fallback: "continue" carries no terms to match, so the
-- tail offers the freshest other sessions instead.
SELECT session_id, title, checkpoint, files, ended_at, project_key,
       param_version
FROM session_digests
WHERE project_key = ? AND session_id != ?
ORDER BY ended_at DESC
LIMIT ?;

-- name: IndexSessionDigest :exec
-- The FTS shadow is a plain FTS5 table (not external-content) so a
-- digest refresh is a keyed delete + insert.
INSERT INTO session_digests_fts (session_id, body) VALUES (?, ?);

-- name: DeleteSessionDigestIndex :exec
DELETE FROM session_digests_fts WHERE session_id = ?;

-- name: SearchSessionDigests :many
-- FTS5 match on the digest body (title + checkpoint + files) joined
-- back to the pointer fields. `body MATCH` --- not
-- `session_digests_fts MATCH` --- because sqlc resolves column MATCH
-- but not table-name MATCH; body is the only indexed column so the
-- two are equivalent here.
SELECT d.session_id, d.title, d.checkpoint, d.files, d.ended_at,
       d.project_key, d.param_version
FROM session_digests_fts f
JOIN session_digests d ON d.session_id = f.session_id
WHERE f.body MATCH ? AND d.project_key = ? AND d.session_id != ?
ORDER BY f.rank LIMIT ?;

-- name: DeleteSessionDigest :exec
DELETE FROM session_digests WHERE session_id = ?;

-- name: GetSessionDigest :one
SELECT session_id, title, checkpoint, files, ended_at, project_key,
       param_version
FROM session_digests
WHERE session_id = ? LIMIT 1;
