-- name: InsertCrushCommit :exec
-- One commit object the harness produced, attributed by HEAD
-- movement at run-record time (#359) -- exact-SHA exclusion replaces
-- the old spelling-patterns-plus-time-skew heuristic.
INSERT OR IGNORE INTO crush_commits (project_key, sha, session_id, created_at)
VALUES (?, ?, ?, ?);

-- name: ListCrushCommitSHAs :many
-- Every commit SHA this project's harness runs produced -- the
-- exact-match exclusion set for external commit evidence (#359).
SELECT sha FROM crush_commits WHERE project_key = ?;

-- name: ListCrushCommitPartitionKeys :many
SELECT DISTINCT project_key FROM crush_commits WHERE project_key != '';

-- name: RekeyCrushCommits :exec
-- Remote-lifecycle re-claim (#266): attributed commits stamped under
-- a stale sibling key ride the same sweep command_memory takes --
-- left behind, they orphan and every one of those commits reads as
-- external acceptance forever. OR IGNORE covers the (project_key,
-- sha) twin: the current key's row wins and the stale-keyed row is
-- skipped, left dead under the old key -- reads are key-scoped, so
-- the residue is inert.
UPDATE OR IGNORE crush_commits SET project_key = ? WHERE project_key = ?;

-- name: LastRecordedRepoHead :one
-- HEAD as of the most recent recorded run -- the catch-up baseline
-- an unstamped run diffs against to attribute its commits, and the
-- retry range when a stamped pre-run head died in the run's own
-- rewrite (#359, #365).
SELECT repo_state FROM command_memory
WHERE project_key = ? AND repo_state != ''
ORDER BY last_at DESC, rowid DESC LIMIT 1;

-- name: InsertPendingAttribution :exec
-- A (prev, post) range whose sweep failed under a held baseline
-- (#372) -- persisted so a later run's drain re-sweeps what no
-- stamped run will. OR IGNORE covers re-persist of the same range:
-- the original row's attempts count and created_at stand.
INSERT OR IGNORE INTO pending_attribution (project_key, prev_sha, post_sha, attempts, created_at)
VALUES (?, ?, ?, 0, ?);

-- name: ListPendingAttribution :many
-- Pending ranges oldest-first for FIFO drain; the sort tiebreak keeps
-- the order deterministic when two ranges share a created_at tick.
SELECT prev_sha, post_sha, attempts FROM pending_attribution
WHERE project_key = ?
ORDER BY created_at ASC, prev_sha ASC, post_sha ASC
LIMIT ?;

-- name: BumpPendingAttributionAttempts :exec
-- A drained range that failed rev-list again -- the bound drops the
-- row at the retry limit, mirroring sweepFail's once-more contract.
UPDATE pending_attribution SET attempts = attempts + 1
WHERE project_key = ? AND prev_sha = ? AND post_sha = ?;

-- name: DeletePendingAttribution :exec
-- A range fully drained, proven dead, or past its retry bound.
DELETE FROM pending_attribution
WHERE project_key = ? AND prev_sha = ? AND post_sha = ?;

-- name: EvictPendingAttribution :exec
-- Cap the pending set -- beyond it the newest rows win and the oldest
-- are abandoned (a bounded miss, not a pile-up of deterministic
-- failures re-queued forever). Rows tied at the boundary tick all
-- survive -- a slight overshoot is cheaper than picking between them.
DELETE FROM pending_attribution
WHERE pending_attribution.project_key = ?
  AND pending_attribution.created_at NOT IN (
    SELECT keep.created_at FROM pending_attribution AS keep
    WHERE keep.project_key = ?
    ORDER BY keep.created_at DESC
    LIMIT ?
  );

-- name: ListPendingAttributionPartitionKeys :many
SELECT DISTINCT project_key FROM pending_attribution WHERE project_key != '';

-- name: RekeyPendingAttribution :exec
-- Remote-lifecycle re-claim (#266): pending rows under a stale
-- sibling key would never drain -- reads are key-scoped. OR IGNORE
-- covers a (project_key, prev_sha, post_sha) twin: the current key's
-- row wins, the stale-keyed row is left dead under the old key.
UPDATE OR IGNORE pending_attribution SET project_key = ? WHERE project_key = ?;
