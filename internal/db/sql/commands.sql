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
    last_session_id,
    last_tool_call_id,
    repo_state,
    suggested,
    project_key,
    param_version
) VALUES (
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    ?
) ON CONFLICT(cmd_norm, cwd, project_key) DO UPDATE SET
    kind = CASE WHEN excluded.last_exit >= 0 THEN excluded.kind ELSE command_memory.kind END,
    -- Interrupted runs carry last_exit = -1: the run is noted but
    -- never overwrites the command's last real verdict -- nor the
    -- session stamp, which must stay with the last verdict's writer
    -- so a denied or killed re-run cannot disown the observing
    -- session (or claim a failing verdict it never saw). The
    -- observation provenance rides the same guard: it describes the
    -- last real verdict, so a verdictless run must not re-stamp it.
    last_exit = CASE WHEN excluded.last_exit >= 0 THEN excluded.last_exit ELSE command_memory.last_exit END,
    last_at = CASE WHEN excluded.last_exit >= 0 THEN excluded.last_at ELSE command_memory.last_at END,
    ok_count = command_memory.ok_count + excluded.ok_count,
    fail_count = command_memory.fail_count + excluded.fail_count,
    last_session_id = CASE WHEN excluded.last_exit >= 0 THEN excluded.last_session_id ELSE command_memory.last_session_id END,
    last_tool_call_id = CASE WHEN excluded.last_exit >= 0 THEN excluded.last_tool_call_id ELSE command_memory.last_tool_call_id END,
    repo_state = CASE WHEN excluded.last_exit >= 0 THEN excluded.repo_state ELSE command_memory.repo_state END,
    suggested = CASE WHEN excluded.last_exit >= 0 THEN excluded.suggested ELSE command_memory.suggested END,
    -- Partition and params-in-force are not verdict properties: the
    -- latest writer's are always authoritative.
    project_key = excluded.project_key,
    param_version = excluded.param_version;

-- name: UpsertFailure :exec
-- One row per (normalized command, directory, error headline)
-- signature. A re-fail refreshes the observation -- headline, file
-- hints, and the observation's provenance all move with the latest
-- failure, not the first.
INSERT INTO failure_memory (
    signature,
    cmd,
    cwd,
    headline,
    files,
    first_seen,
    last_seen,
    session_id,
    tool_call_id,
    repo_state,
    suggested,
    project_key,
    param_version
) VALUES (
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
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
    -- first_seen stamps the open epoch, not the original birth: a
    -- resurrected row (was resolved) counts as newly introduced, a
    -- continuously-open re-fail keeps its first observation.
    first_seen = CASE WHEN failure_memory.resolved_in != '' THEN excluded.first_seen ELSE failure_memory.first_seen END,
    last_seen = excluded.last_seen,
    session_id = excluded.session_id,
    tool_call_id = excluded.tool_call_id,
    repo_state = excluded.repo_state,
    suggested = excluded.suggested,
    project_key = excluded.project_key,
    param_version = excluded.param_version,
    resolved_in = '',
    -- A re-fail clears the old resolution lineage: the row's open
    -- epoch is new, so who closed the previous epoch is history.
    resolved_call = '';

-- name: ResolveFailuresForCommand :exec
-- A clean run of a normalized command resolves its open failure rows
-- in the same directory -- "go test ./..." passing in packages/web
-- does not close packages/api's failure. resolved_call names the
-- call that carried the clean verdict, where detectable. The
-- resolution is partition-scoped like every read: one project's
-- green run cannot close another's observation.
UPDATE failure_memory SET
    resolved_in = ?,
    resolved_call = ?
WHERE cmd = ? AND cwd = ? AND resolved_in = '' AND project_key = ?;

-- name: ListUnclaimedFailures :many
-- Legacy failure rows awaiting a partition claim (pre-provenance
-- writes). The claim re-keys them Go-side because the signature hash
-- itself carries project_key -- a column UPDATE alone would leave a
-- stale PK, and the next occurrence of the same failure would open a
-- second row.
SELECT signature, cmd, cwd, headline, first_seen
FROM failure_memory WHERE project_key = '';

-- name: ListFailuresByKey :many
-- Failure rows in one partition -- the remote-lifecycle re-claim
-- (#266) moves them off a stale key onto the current one.
SELECT signature, cmd, cwd, headline, first_seen
FROM failure_memory WHERE project_key = ?;

-- name: ListFailurePartitionKeys :many
SELECT DISTINCT project_key FROM failure_memory WHERE project_key != '';

-- name: ListCommandPartitionKeys :many
SELECT DISTINCT project_key FROM command_memory WHERE project_key != '';

-- name: GetFailureMeta :one
SELECT signature, first_seen FROM failure_memory WHERE signature = ?;

-- name: RekeyFailurePartition :exec
-- Claim one legacy row onto its partitioned signature.
UPDATE failure_memory SET signature = ?, project_key = ? WHERE signature = ?;

-- name: MergeFailureFirstSeen :exec
-- A claimed row colliding with an already-partitioned twin keeps the
-- twin (fresher provenance) but the failure's true age survives --
-- the caller passes min(twin.first_seen, legacy.first_seen).
UPDATE failure_memory SET first_seen = ? WHERE signature = ?;

-- name: DeleteFailure :exec
DELETE FROM failure_memory WHERE signature = ?;

-- name: DeleteCommandConflicts :exec
-- Re-key collision on command_memory: project_key is PK material, so
-- moving a row from source key to target key conflicts when the
-- (cmd_norm, cwd, target) twin already exists. The target twin wins
-- -- fresher provenance -- so the stale row goes before the re-key.
DELETE FROM command_memory AS stale WHERE stale.project_key = ? AND EXISTS (
    SELECT 1 FROM command_memory twin
    WHERE twin.project_key = ?
        AND twin.cmd_norm = stale.cmd_norm
        AND twin.cwd = stale.cwd
);

-- name: RekeyCommandPartition :exec
UPDATE command_memory SET project_key = ? WHERE project_key = ?;

-- name: ListRecentCommands :many
-- last_at is millisecond-granularity so re-runs order by recency;
-- rowid settles ties for rows written in the same millisecond.
-- project_key is an admissibility clause, not a ranking signal: a
-- candidate from another partition is inadmissible before relevance
-- ever runs.
SELECT * FROM command_memory WHERE project_key = sqlc.arg(project_key) ORDER BY last_at DESC, rowid DESC LIMIT sqlc.arg(row_limit);

-- name: ListOpenFailures :many
SELECT * FROM failure_memory WHERE resolved_in = '' AND project_key = sqlc.arg(project_key) ORDER BY last_seen DESC, rowid DESC LIMIT sqlc.arg(row_limit);

-- name: ListSessionOpenFailures :many
-- Open failure rows whose commands the given session (or one of its
-- task-tool child sessions) last ran and last failed: the reconcile
-- edge's "observed and left open" set. A sub-agent's bash records
-- under the child session ID; without the children subquery those
-- rows would reconcile to no one -- the child never scans and the
-- parent's delegation produced the mess. One level only, matching
-- the task tool's nesting depth. command_memory's last_session_id
-- is last-writer, so a row another session re-ran more recently
-- drops out of this session's set even while it stays open; a
-- concurrently opened row the run never invoked can never flag here.
SELECT f.*
FROM failure_memory f
INNER JOIN command_memory c
    ON c.cmd_norm = f.cmd
    AND c.cwd = f.cwd
WHERE f.resolved_in = ''
    AND f.project_key = sqlc.arg(project_key)
    AND c.project_key = sqlc.arg(project_key)
    AND c.last_session_id IN (
        SELECT id FROM sessions
        WHERE id = sqlc.arg(session_id) OR parent_session_id = sqlc.arg(session_id)
    )
    AND c.last_exit > 0
ORDER BY f.last_seen DESC, f.rowid DESC
-- Same bound as the tail's fetch pool -- a session can observe more
-- distinct commands than this only pathologically, and the retry
-- prompt renders at most ten.
LIMIT 50;

-- name: ListResolvedFailures :many
-- Resolved rows are knowledge, not warnings: the failure signature
-- and when it last saw a clean run. Ordered by last_seen (the last
-- failing observation), not resolution time -- the row's freshness
-- is still about when the failure was last real.
SELECT * FROM failure_memory WHERE resolved_in != '' AND project_key = sqlc.arg(project_key) ORDER BY last_seen DESC, rowid DESC LIMIT sqlc.arg(row_limit);
