-- +goose Up
-- Display-name change cooldown (once per 30 days, enforced by the
-- ChangeDisplayName statement's predicate). NULL means the name has never
-- been changed — the registration name does not start the clock.
ALTER TABLE users ADD COLUMN display_name_changed_at timestamptz;

-- +goose Down
ALTER TABLE users DROP COLUMN display_name_changed_at;
