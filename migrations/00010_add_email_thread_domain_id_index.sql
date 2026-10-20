-- +goose NO TRANSACTION
-- +goose Up
-- Unique (domain_id, id) for references with the domain. Built concurrently; a
-- leftover invalid index of an interrupted run is dropped first.
DROP INDEX CONCURRENTLY IF EXISTS email.thread_domain_id_uidx;
CREATE UNIQUE INDEX CONCURRENTLY thread_domain_id_uidx ON email.thread (domain_id, id);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS email.thread_domain_id_uidx;
