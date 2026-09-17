-- name: RecordCollapsedTurn :execrows
-- One row per collapsed prior turn; INSERT OR IGNORE makes the write
-- idempotent across renders and across processes sharing the session
-- DB, so callers count a turn only when this reports a new row.
INSERT OR IGNORE INTO collapsed_turns (
    session_id,
    turn_number,
    events,
    created_at
) VALUES (
    ?, ?, ?, strftime('%s', 'now')
);

-- name: BumpSessionCounter :exec
INSERT INTO session_counters (
    session_id,
    name,
    value
) VALUES (
    ?, ?, ?
)
ON CONFLICT (session_id, name)
DO UPDATE SET value = value + excluded.value;

-- name: GetCollapsedTurnStats :one
SELECT
    COUNT(*) AS turns,
    COALESCE(SUM(events), 0) AS events,
    COUNT(DISTINCT session_id) AS sessions
FROM collapsed_turns;

-- name: ListSessionCounters :many
SELECT
    name,
    COALESCE(SUM(value), 0) AS value,
    COUNT(DISTINCT session_id) AS sessions
FROM session_counters
GROUP BY name;

