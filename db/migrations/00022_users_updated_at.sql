-- +goose Up
--
-- users grew its first mutable columns in 00018 (profile_public /
-- keyboard_public, written by PATCH /me/settings), which made "when did this
-- row last change" a question the schema could no longer answer. updated_at
-- records it.
--
-- Maintained by the WRITERS, not by a trigger: every UPDATE of users sets
-- updated_at = now() itself (today that is exactly one statement,
-- UpdateUserSettings in internal/auth/queries.sql). This codebase has no
-- triggers, and a hidden mechanism for one column on one low-write table is a
-- worse trade than a review rule — the same reasoning that keeps layering
-- enforced by review rather than tooling. If the write sites multiply, a
-- trigger is the known upgrade path.
--
-- The backfill sets updated_at = created_at rather than leaving the ADD
-- COLUMN's now(): for a row nothing has touched, "last changed when created"
-- is a fact, while the migration's own timestamp would be noise dressed as
-- one. New rows agree by construction — both defaults are the same
-- transaction's now().
ALTER TABLE users
    ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();

UPDATE users SET updated_at = created_at;

-- +goose Down
ALTER TABLE users DROP COLUMN updated_at;
