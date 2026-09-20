-- name: InsertEdgeFiring :execrows
-- One row per edge per run boundary; INSERT OR IGNORE makes the write
-- idempotent within a boundary, so callers count a firing only when
-- this reports a new row.
INSERT OR IGNORE INTO edge_firings (
    session_id,
    turn_seq,
    edge,
    variant,
    repair_attempts,
    trigger_detail,
    outcome,
    run_stamp,
    created_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, strftime('%s', 'now')
);

-- name: GetEdgeFiringStats :many
SELECT
    edge,
    outcome,
    COUNT(*) AS firings,
    COUNT(DISTINCT session_id) AS sessions
FROM edge_firings
GROUP BY edge, outcome
ORDER BY edge, outcome;
