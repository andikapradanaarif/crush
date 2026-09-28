-- name: RecordFileRead :exec
INSERT INTO read_files (
    session_id,
    path,
    read_at
) VALUES (
    ?,
    ?,
    strftime('%s', 'now')
) ON CONFLICT(path, session_id) DO UPDATE SET
    read_at = excluded.read_at;

-- name: GetFileRead :one
SELECT * FROM read_files
WHERE session_id = ? AND path = ? LIMIT 1;

-- name: ListHotReadFiles :many
-- Project-wide file heat: files prior sessions in this workspace
-- touched, ranked by persistence (how many sessions read the file)
-- then recency. The current session is excluded -- its files are the
-- working set, not heat; a file only this session read would carry
-- a sessions count of 1 noise-signal anyway.
SELECT path,
    COUNT(DISTINCT session_id) AS sessions,
    MAX(read_at) AS last_read
FROM read_files
WHERE session_id != ?
GROUP BY path
ORDER BY sessions DESC, last_read DESC
LIMIT ?;

-- name: ListSessionReadFiles :many
SELECT * FROM read_files
WHERE session_id = ?
ORDER BY read_at DESC;
