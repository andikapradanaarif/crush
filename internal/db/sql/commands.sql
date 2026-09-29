-- name: UpsertCommandRun :exec
-- Project-scoped command ledger: one row per normalized command,
-- shared across sessions. ok/fail counts merge additively so the row
-- is the running tally, not a per-session sample.
INSERT INTO command_memory (
    cmd_norm,
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
    ?
) ON CONFLICT(cmd_norm) DO UPDATE SET
    kind = excluded.kind,
    last_exit = excluded.last_exit,
    last_at = excluded.last_at,
    ok_count = command_memory.ok_count + excluded.ok_count,
    fail_count = command_memory.fail_count + excluded.fail_count,
    last_session_id = excluded.last_session_id;

-- name: UpsertFailure :exec
-- One row per (normalized command, error headline) signature. A
-- re-fail after resolution reopens the row -- the same signature
-- failing again is the same failure, not a new one.
INSERT INTO failure_memory (
    signature,
    cmd,
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
    ?
) ON CONFLICT(signature) DO UPDATE SET
    last_seen = excluded.last_seen,
    resolved_in = '';

-- name: ResolveFailuresForCommand :exec
-- A clean run of a normalized command resolves its open failure rows:
-- "go test ./..." passing closes every open failure of that command.
UPDATE failure_memory SET
    resolved_in = ?
WHERE cmd = ? AND resolved_in = '';

-- name: ListRecentCommands :many
-- rowid breaks same-second ties: the later insert is the later run.
SELECT * FROM command_memory ORDER BY last_at DESC, rowid DESC LIMIT ?;

-- name: ListOpenFailures :many
SELECT * FROM failure_memory WHERE resolved_in = '' ORDER BY last_seen DESC, rowid DESC LIMIT ?;
