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

-- name: ClaimMemoryPartition :exec
-- Backfill policy for pre-provenance rows: a project-local store's
-- legacy rows belong to this project's partition, so empty
-- project_key claims on first open. Rows already stamped stay put
-- -- a store genuinely shared across projects (an absolute
-- data_directory) keeps its foreign rows foreign. OR REPLACE covers
-- the rare collision of a claimed row meeting an already-partitioned
-- twin: the partitioned row carries fresher provenance, so it wins.
UPDATE OR REPLACE failure_memory SET project_key = ? WHERE project_key = '';

-- name: ClaimCommandPartition :exec
UPDATE OR REPLACE command_memory SET project_key = ? WHERE project_key = '';

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
