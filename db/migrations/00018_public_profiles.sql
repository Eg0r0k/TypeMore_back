-- +goose Up
-- Public profiles (docs/PROFILE.md, "Public profiles"): two per-account
-- switches, both owned by the account and read by the public /users/{name}
-- surface.
--
--   * profile_public  — whether the profile page answers strangers at all.
--     DEFAULT TRUE: the product decision is open-by-default, and the flag
--     shipped together with the surface it gates, so no account existed with
--     an expectation of privacy that this default would betray.
--   * keyboard_public — the keyboard portrait's OWN switch, independent of the
--     general one. DEFAULT FALSE, deliberately: per-key error rates and
--     inter-key timings are effectively biometric (they identify and profile a
--     person's motor behaviour — docs/PROFILE.md, "Privacy"), so they never
--     ship on a public surface unless their owner turned them on themselves.
--     A closed profile hides the portrait regardless of this flag.
--
-- Neither flag touches the leaderboards: a closed profile stays ranked under
-- its name, and a run holding a board slot stays watchable (the public replay
-- pair in internal/runs/queries.sql carries that rule). What closes is the
-- aggregated profile page, not a result its owner put into a public ranking.
ALTER TABLE users
    ADD COLUMN profile_public  boolean NOT NULL DEFAULT true,
    ADD COLUMN keyboard_public boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE users
    DROP COLUMN keyboard_public,
    DROP COLUMN profile_public;
