-- +goose Up
-- +goose StatementBegin
-- Run-boundary edge firing records. One row per edge per evaluated
-- boundary: the primary key is the dedupe for idempotent writes within
-- a boundary (e.g. a mid-write crash), and turn_seq is the absolute
-- ordinal of the run's initiating user message — a boundary ordinal,
-- not a user-turn ordinal: repair retries and summarize-continue
-- requeues persist another user message, so one logical user turn can
-- span several turn_seq values.
-- run_stamp is a join column for run:<stamp> checkpoint tags, not key
-- material: the stamp is a per-process random epoch + sequence, neither
-- deterministic nor durable per logical turn.
CREATE TABLE IF NOT EXISTS edge_firings (
    session_id TEXT NOT NULL,
    turn_seq INTEGER NOT NULL,
    edge TEXT NOT NULL,
    variant TEXT NOT NULL DEFAULT '',
    repair_attempts INTEGER NOT NULL DEFAULT 0,
    trigger_detail TEXT NOT NULL DEFAULT '',
    outcome TEXT NOT NULL,
    run_stamp INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,  -- Unix timestamp in seconds
    PRIMARY KEY (session_id, turn_seq, edge),
    FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS edge_firings;
-- +goose StatementEnd
