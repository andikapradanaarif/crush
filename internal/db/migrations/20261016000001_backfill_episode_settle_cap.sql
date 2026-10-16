-- +goose Up
-- +goose StatementBegin
-- #354 companion: rows labeled before the anchor fix carry
-- settle_at = labeled_at + 24h — a wider cap than documented.
-- Tighten in-flight rows to the insert's anchor (created_at +
-- 24h). Unsettled only: frozen rows' windows are spent. The
-- settle_at != 0 guard leaves legacy pre-column rows on the
-- documented windowEnd fallback. The only direction is tighter —
-- a row whose cap lands in the past is simply due now.
UPDATE referent_episodes
SET settle_at = created_at + 86400000
WHERE settled_at IS NULL AND settle_at != 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Irreversible: the original labeled_at stamps are not kept.
SELECT 1;
-- +goose StatementEnd
