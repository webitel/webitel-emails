-- +goose NO TRANSACTION
-- +goose Up
-- Scans the table under SHARE UPDATE EXCLUSIVE, so reads and writes go on. A
-- repeat after a failure is safe: a valid constraint is not checked again.
ALTER TABLE email.thread VALIDATE CONSTRAINT thread_workspace_version_valid;

-- +goose Down
-- The constraint is dropped with its column by 00014.
SELECT 1;
