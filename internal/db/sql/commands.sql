-- name: UpsertCommandRun :exec
-- Project-scoped command ledger: one row per normalized command,
-- shared across sessions. ok/fail counts merge additively so the row
-- is the running tally, not a per-session sample.
INSERT INTO command_memory (
    cmd_norm,
    cwd,
    kind,
    last_exit,
    last_at,
    ok_count,
    fail_count,
    last_session_id
) VALUES (
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    ?
) ON CONFLICT(cmd_norm, cwd) DO UPDATE SET
    kind = excluded.kind,
    -- Interrupted runs carry last_exit = -1: the run is noted but
    -- never overwrites the command's last real verdict.
    last_exit = CASE WHEN excluded.last_exit >= 0 THEN excluded.last_exit ELSE command_memory.last_exit END,
    last_at = excluded.last_at,
    ok_count = command_memory.ok_count + excluded.ok_count,
    fail_count = command_memory.fail_count + excluded.fail_count,
    last_session_id = excluded.last_session_id;

-- name: UpsertFailure :exec
-- One row per (normalized command, directory, error headline)
-- signature. A re-fail refreshes the observation -- headline and
-- file hints move with the latest failure, not the first.
INSERT INTO failure_memory (
    signature,
    cmd,
    cwd,
    headline,
    files,
    first_seen,
    last_seen
) VALUES (
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    ?
) ON CONFLICT(signature) DO UPDATE SET
    headline = excluded.headline,
    files = excluded.files,
    last_seen = excluded.last_seen,
    resolved_in = '';

-- name: ResolveFailuresForCommand :exec
-- A clean run of a normalized command resolves its open failure rows
-- in the same directory -- "go test ./..." passing in packages/web
-- does not close packages/api's failure.
UPDATE failure_memory SET
    resolved_in = ?
WHERE cmd = ? AND cwd = ? AND resolved_in = '';

-- name: ListRecentCommands :many
-- last_at is millisecond-granularity so re-runs order by recency;
-- rowid settles ties for rows written in the same millisecond.
SELECT * FROM command_memory ORDER BY last_at DESC, rowid DESC LIMIT ?;

-- name: ListOpenFailures :many
SELECT * FROM failure_memory WHERE resolved_in = '' ORDER BY last_seen DESC, rowid DESC LIMIT ?;
