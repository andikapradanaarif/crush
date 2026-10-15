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

-- name: InsertReferentEpisode :exec
-- One judged vague-prompt observation (#165). INSERT OR IGNORE makes
-- the (session_id, source_message_id, target) unique key the retry
-- idempotency point: a repair-chain re-run of the turn-context pass
-- re-derives the same episode and must not count twice.
INSERT OR IGNORE INTO referent_episodes (
    phrase, target, session_id, source_message_id, tool_call_id,
    repo_state, memory_suggested, verdict, project_key, param_version,
    created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: CountCleanReferentAcceptances :one
-- Evidence for a phrase->target mapping: accepted episodes the memory
-- tail did not suggest, counted by distinct session so a session
-- re-deriving its own mapping cannot self-promote.
SELECT COUNT(DISTINCT session_id) FROM referent_episodes
WHERE project_key = ? AND phrase = ? AND target = ?
    AND verdict = 'accepted' AND memory_suggested = 0;

-- name: PromoteReferent :exec
-- A mapping that cleared the acceptance floor earns its row; further
-- clean acceptances keep scoring it (hits is evidence mass, not
-- capped at the threshold). The insert carries the true acceptance
-- count that crossed the floor -- seeding 1 would understate the
-- evidence that promoted it.
INSERT INTO referent_memory (phrase, target, hits, last_at, project_key, param_version)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (phrase, target, project_key) DO UPDATE SET
    hits = hits + 1,
    last_at = excluded.last_at,
    param_version = excluded.param_version;

-- name: ListReferentsForPhrases :many
-- Promoted candidates whose learned phrase appears among the turn's
-- extracted phrases -- exact-match on the normalized phrase keeps the
-- render honest (a fuzzy hit would be a guess, not learned evidence).
-- Bare placeholders only: numbered ?N collides with the slice's
-- variable-length expansion at bind time.
SELECT * FROM referent_memory
WHERE project_key = ? AND phrase IN (sqlc.slice('phrases'))
ORDER BY hits DESC, last_at DESC
LIMIT ?;

-- name: LabelReferentEpisode :exec
-- Stamp the artifact signals computable at episode-record time
-- (#294/#340): the file's post-edit hash (diagnostic), the added-
-- lines survival fingerprint, the judged turn's cost, the session's
-- test verdict, and the observation window's +24h edge. The key
-- clause is the episode unique key -- the same idempotency point
-- the insert uses, so a repair-chain re-run re-stamps the same row
-- instead of drifting.
UPDATE referent_episodes SET label_target_hash = ?, label_wrong_target = ?, label_steps = ?, label_tokens = ?, label_tests_green = ?, labeled_at = ?, label_added_lines = ?, settle_at = ? WHERE session_id = ? AND source_message_id = ? AND target = ?;

-- name: ListSessionTestVerdicts :many
-- kind='test' ledger rows whose last real run belongs to this
-- session (or a task-tool child) -- the session-end test evidence
-- (#294). last_exit is the row's last verdict, not this run's, so a
-- command the session ran only early counts toward session-end
-- state the way the ledger models it. Matches the
-- ListSessionOpenFailures session-with-children shape.
SELECT c.cmd_norm, c.last_exit FROM command_memory c
WHERE c.project_key = ?
    AND c.kind = 'test'
    AND c.last_exit >= 0
    AND c.last_session_id IN (
        SELECT id FROM sessions WHERE id = ? OR parent_session_id = ?
    );

-- name: ListUnsettledReferentLabels :many
-- Episodes whose observation window has closed but whose label has
-- not frozen yet (#340): settle_at elapsed (the +24h cap) or a newer
-- session exists in the project -- the two settlement triggers the
-- fixed-window rule defines. Oldest first so the backlog amortizes.
-- Rows never become un-settleable: settlement writes its verdict
-- once and freezes.
SELECT id, target, session_id, created_at, settle_at, label_added_lines
FROM referent_episodes
WHERE project_key = ?
    AND settled_at IS NULL
    AND (settle_at <= ?
         OR EXISTS (SELECT 1 FROM sessions s
                    WHERE s.id != referent_episodes.session_id
                      AND s.parent_session_id IS NULL
                      AND s.created_at * 1000 > referent_episodes.created_at))
ORDER BY created_at ASC
LIMIT ?;

-- name: NextSessionStartMillis :one
-- The next session's start after a given episode -- the inner edge of
-- the settlement window (#340). Sessions stamp created_at in seconds;
-- episodes use millis. NULL when no newer session exists (the 24h cap
-- alone bounds the window then). parent_session_id IS NULL keeps
-- sub-agent child sessions out: a delegated task is work inside the
-- parent turn, not a session boundary.
SELECT MIN(created_at) * 1000 FROM sessions
WHERE created_at * 1000 > ? AND id != ? AND parent_session_id IS NULL;

-- name: ListCrushCommitRuns :many
-- Timestamps (millis) of git-commit commands this project ran through
-- the harness inside a window (#340): a commit whose author-time sits
-- within the skew bound of one of these was agent-made, and does not
-- count as external acceptance evidence. cmd_norm keeps the whole
-- invocation, so every distinct commit message is its own row; the
-- known limit is that re-running an identical message keeps only the
-- latest last_at.
SELECT last_at FROM command_memory
WHERE project_key = ?
    AND cmd_norm LIKE 'git commit%'
    AND last_at >= ? AND last_at <= ?;

-- name: SettleReferentEpisode :exec
-- Freeze one episode's label (#340): the write happens once, at
-- settlement -- committed counts only external commits inside the
-- window, hash_changed means the recorded added-lines diverged, and
-- settled_at is the freeze mark that takes the row out of the scan
-- and into the fold/prior evidence predicates.
UPDATE referent_episodes SET label_committed = ?, label_hash_changed = ?, label_tests_green = ?, settled_at = ?, labeled_at = ? WHERE id = ?;

-- name: GetProjectRate :one
-- The stored posterior mass for one (project, signal) pair (#296)
-- plus the fold cursor -- absent row means no evidence yet counted.
SELECT alpha, beta, last_event_id, last_session_id FROM project_rates
WHERE project_key = ? AND signal = ?;

-- name: UpsertProjectRate :exec
INSERT INTO project_rates (project_key, signal, alpha, beta, last_event_id, last_session_id, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (project_key, signal) DO UPDATE SET
    alpha = excluded.alpha,
    beta = excluded.beta,
    last_event_id = excluded.last_event_id,
    last_session_id = excluded.last_session_id,
    updated_at = excluded.updated_at;

-- name: ListReferentRateEvents :many
-- Settled, not-yet-folded referent episodes (#296/#340), oldest
-- first. Membership is the rate_folded mark, not an id watermark.
-- Since #340 only settled rows are evidence: settlement froze the
-- windowed commit read, the hunk-survival verdict, and the cue
-- verdict into one fixed observation -- folding unsettled rows would
-- re-mix label vintages the review flagged. The outcome rule still
-- lives in Go so the prior pool and the fold share one definition.
SELECT id, session_id, verdict, label_committed, label_hash_changed
FROM referent_episodes
WHERE project_key = ?
    AND rate_folded = 0
    AND settled_at IS NOT NULL
ORDER BY id ASC
LIMIT ?;

-- name: MarkReferentEventsFolded :exec
-- Stamp the fold's membership mark on the ids this pass counted.
UPDATE referent_episodes SET rate_folded = 1
WHERE id IN (sqlc.slice('ids'));

-- name: ListReferentPriorEvents :many
-- Pooled outcome evidence for the empirical-Bayes prior (#296) --
-- LEAVE-ONE-OUT over every OTHER project: counting the querying
-- project's own episodes in the prior and again as local mass
-- roughly doubles their weight, which is least conservative
-- exactly where data is scarcest (the single-project store).
SELECT verdict, label_committed, label_hash_changed
FROM referent_episodes
WHERE project_key != ?
    AND settled_at IS NOT NULL;
-- name: ListReferentCalibrationEpisodes :many
-- Settled referent episodes in calibration form (#295/#340):
-- settlement is the evidence predicate -- one fixed observation per
-- episode -- plus the mapping context (phrase/target) and the
-- suggestion flag, so the calibrator derives renderability under
-- each candidate floor and stratifies one draw per session. Oldest
-- first: per-session stratification picks the earliest episode and
-- must be deterministic.
SELECT id, phrase, target, session_id, verdict, label_committed,
    label_hash_changed, memory_suggested
FROM referent_episodes
WHERE project_key = ?
    AND settled_at IS NOT NULL
ORDER BY id ASC;

-- name: InsertTurnOutcome :exec
-- Open one turn's outcome row (#340), idempotent on the
-- (session_id, turn_id) unique key -- turn_id is the turn's
-- user-message ID, stable under compaction where an ordinal would
-- shift; a repair-chain re-derivation or a backfill racing a live
-- write re-inserts nothing. verdict stays NULL until settlement;
-- next_verdict captures the cue-word judgment the following user
-- prompt carried at record time.
INSERT OR IGNORE INTO turn_outcomes
    (session_id, turn_id, project_key, edited_paths, edit_hunks,
     commands, next_verdict, settle_at, param_version, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListUnsettledTurnOutcomes :many
-- Turn rows whose window closed without settling (#340): the same
-- dual trigger as episodes -- the +24h cap elapsed or a newer session
-- exists. Oldest first.
SELECT id, session_id, turn_id, edited_paths, edit_hunks, commands,
    next_verdict, settle_at, created_at
FROM turn_outcomes
WHERE project_key = ?
    AND settled_at IS NULL
    AND (settle_at <= ?
         OR EXISTS (SELECT 1 FROM sessions s
                    WHERE s.id != turn_outcomes.session_id
                      AND s.parent_session_id IS NULL
                      AND s.created_at * 1000 > turn_outcomes.created_at))
ORDER BY created_at ASC
LIMIT ?;

-- name: SettleTurnOutcome :exec
-- Freeze one turn row's verdict (#340). Same contract as episodes:
-- written once, never re-derived.
UPDATE turn_outcomes SET verdict = ?, settled_at = ? WHERE id = ?;

-- name: ListRecentSessions :many
-- Sessions other than the live one, newest first -- the backfill
-- corpus for turn outcomes (#340). Sessions stamp created_at in
-- seconds. parent_session_id IS NULL keeps sub-agent sessions out:
-- their "user prompts" are delegation text, not user verdicts.
SELECT id, created_at FROM sessions
WHERE id != ? AND parent_session_id IS NULL
ORDER BY created_at DESC
LIMIT ?;

-- name: HasTurnOutcome :one
-- Whether a (session, turn_id) row already exists -- the backfill's
-- skip check before parsing a turn's messages.
SELECT COUNT(*) FROM turn_outcomes WHERE session_id = ? AND turn_id = ?;
