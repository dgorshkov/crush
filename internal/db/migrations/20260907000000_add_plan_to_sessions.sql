-- +goose Up
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN plan TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN plan;
-- +goose StatementEnd
