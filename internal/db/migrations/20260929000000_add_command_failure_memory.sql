-- +goose Up
-- +goose StatementBegin
-- Project memory of what ran and what failed. Both tables are
-- project-scoped and deliberately carry no session foreign key: the
-- memory exists to outlive individual sessions (a deleted session
-- must not cascade-delete what later sessions learned from it).
-- last_session_id / resolved_in are provenance, not key material.

-- One row per (normalized command, directory): the project's command
-- ledger, scoped the same way failure_memory is scoped — "npm test"
-- in packages/api and packages/web are different rows, and "run the
-- tests" resolves against a row that knows where it ran.
CREATE TABLE IF NOT EXISTS command_memory (
    cmd_norm        TEXT NOT NULL,
    cwd             TEXT NOT NULL DEFAULT '',
    kind            TEXT NOT NULL DEFAULT 'other',
    last_exit       INTEGER NOT NULL,
    last_at         INTEGER NOT NULL,  -- Unix timestamp in milliseconds
    ok_count        INTEGER NOT NULL DEFAULT 0,
    fail_count      INTEGER NOT NULL DEFAULT 0,
    last_session_id TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (cmd_norm, cwd)
);

-- One row per failure signature (normalized command + directory +
-- redacted error headline). cwd is workspace-relative so "npm test"
-- in packages/api and packages/web are different rows: the same
-- command failing in a sibling directory is a different failure, and
-- a green run only resolves the open failures of its own directory.
-- resolved_in records the session where the same command next exited
-- clean in the same directory; empty means the failure is open.
CREATE TABLE IF NOT EXISTS failure_memory (
    signature   TEXT NOT NULL PRIMARY KEY,
    cmd         TEXT NOT NULL,
    cwd         TEXT NOT NULL DEFAULT '',
    headline    TEXT NOT NULL,
    files       TEXT NOT NULL DEFAULT '[]',
    first_seen  INTEGER NOT NULL,  -- Unix timestamp in milliseconds
    last_seen   INTEGER NOT NULL,  -- Unix timestamp in milliseconds
    resolved_in TEXT NOT NULL DEFAULT ''
);

-- Resolution and open-failure listing both filter/scan these shapes;
-- failure_memory is unbounded, so keep them indexed.
CREATE INDEX IF NOT EXISTS idx_failure_memory_cmd_cwd
    ON failure_memory(cmd, cwd);
CREATE INDEX IF NOT EXISTS idx_failure_memory_open
    ON failure_memory(last_seen) WHERE resolved_in = '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS failure_memory;
DROP TABLE IF EXISTS command_memory;
-- +goose StatementEnd
