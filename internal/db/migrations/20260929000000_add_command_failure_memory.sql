-- +goose Up
-- +goose StatementBegin
-- Project memory of what ran and what failed. Both tables are
-- project-scoped and deliberately carry no session foreign key: the
-- memory exists to outlive individual sessions (a deleted session
-- must not cascade-delete what later sessions learned from it).
-- last_session_id / resolved_in are provenance, not key material.

-- One row per normalized command: the project's command ledger.
-- "run the tests" resolves against the most recent kind=test row.
CREATE TABLE IF NOT EXISTS command_memory (
    cmd_norm        TEXT NOT NULL PRIMARY KEY,
    kind            TEXT NOT NULL DEFAULT 'other',
    last_exit       INTEGER NOT NULL,
    last_at         INTEGER NOT NULL,  -- Unix timestamp in seconds
    ok_count        INTEGER NOT NULL DEFAULT 0,
    fail_count      INTEGER NOT NULL DEFAULT 0,
    last_session_id TEXT NOT NULL DEFAULT ''
);

-- One row per failure signature (normalized command + redacted error
-- headline). resolved_in records the session where the same command
-- next exited clean; empty means the failure is open.
CREATE TABLE IF NOT EXISTS failure_memory (
    signature   TEXT NOT NULL PRIMARY KEY,
    cmd         TEXT NOT NULL,
    headline    TEXT NOT NULL,
    files       TEXT NOT NULL DEFAULT '[]',
    first_seen  INTEGER NOT NULL,  -- Unix timestamp in seconds
    last_seen   INTEGER NOT NULL,  -- Unix timestamp in seconds
    resolved_in TEXT NOT NULL DEFAULT ''
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS failure_memory;
DROP TABLE IF EXISTS command_memory;
-- +goose StatementEnd
