-- +goose Up
CREATE SCHEMA email;

-- +goose Down
DROP SCHEMA IF EXISTS email;
