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

-- LastRecordedRepoHead reads the latest repo_state per project on
-- every RecordRun; command_memory's PK puts project_key last, so
-- without a covering index the baseline read is a full scan per
-- recorded command.
CREATE INDEX IF NOT EXISTS idx_command_memory_project_head
    ON command_memory (project_key, last_at);

-- +goose Down
DROP INDEX IF EXISTS idx_command_memory_project_head;
DROP TABLE IF EXISTS crush_commits;
