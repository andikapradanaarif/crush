-- +goose Up
-- +goose StatementBegin
-- Prior-turn collapse telemetry. collapse is a render-time transform
-- that deliberately leaves no stored mark, so the counters persist in
-- their own tables: one row per collapsed turn (the PRIMARY KEY is the
-- dedupe — a turn counts once no matter how many renders or processes
-- collapse it) plus a generic per-session counter table for scalar
-- signals like result: recalls into prior turns.
CREATE TABLE IF NOT EXISTS collapsed_turns (
    session_id TEXT NOT NULL,
    turn_number INTEGER NOT NULL,
    events INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,  -- Unix timestamp in seconds
    PRIMARY KEY (session_id, turn_number),
    FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS session_counters (
    session_id TEXT NOT NULL,
    name TEXT NOT NULL,
    value INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (session_id, name),
    FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS session_counters;
DROP TABLE IF EXISTS collapsed_turns;
-- +goose StatementEnd
