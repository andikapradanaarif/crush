-- +goose Up
-- Commit ranges whose attribution sweep failed under a held baseline
-- (#372). A stamped run's (pre, post) range is unique — a transient
-- rev-list failure that holds the baseline orphans it permanently:
-- the next stamped run's pre sits above it and no stamped run ever
-- re-sweeps. Later successful runs drain these rows: each range gets
-- its own bounded rev-list, drains insert into crush_commits, and the
-- row is deleted — or dropped after the retry bound when the range
-- proves deterministic.
CREATE TABLE IF NOT EXISTS pending_attribution (
    project_key TEXT NOT NULL,
    prev_sha    TEXT NOT NULL,
    post_sha    TEXT NOT NULL,
    attempts    INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (project_key, prev_sha, post_sha)
) WITHOUT ROWID;

-- +goose Down
DROP TABLE IF EXISTS pending_attribution;
