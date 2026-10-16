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

-- name: LastRecordedRepoHead :one
-- HEAD as of the most recent recorded run -- the pre-run baseline a
-- HEAD-moving run diffs against to attribute its commits (#359).
SELECT repo_state FROM command_memory
WHERE project_key = ? AND repo_state != ''
ORDER BY last_at DESC, rowid DESC LIMIT 1;

