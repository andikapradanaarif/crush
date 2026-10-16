-- +goose Up
-- Commits the harness produced, attributed by HEAD movement at
-- RecordRun time (rev-list prevHead..headAfter) -- not by command
-- spelling, so rebase/merge/cherry-pick/am/commit-tree, sudo-prefixed
-- and chained forms all land. Settled evidence excludes these SHAs
-- from external acceptance; an agent commit can never self-certify
-- (#359).
CREATE TABLE IF NOT EXISTS crush_commits (
    project_key TEXT NOT NULL,
    sha         TEXT NOT NULL,
    session_id  TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (project_key, sha)
) WITHOUT ROWID;

-- +goose Down
DROP TABLE IF EXISTS crush_commits;
