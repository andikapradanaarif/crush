-- +goose Up
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN permission_skip INTEGER;
ALTER TABLE sessions ADD COLUMN model_override TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN permission_skip;
ALTER TABLE sessions DROP COLUMN model_override;
-- +goose StatementEnd
