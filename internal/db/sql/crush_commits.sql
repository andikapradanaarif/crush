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
