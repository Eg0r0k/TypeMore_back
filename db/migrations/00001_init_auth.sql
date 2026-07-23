-- +goose Up
-- Auth + identity schema. Users are separate from the credentials/identities
-- that authenticate them, so linking a second provider to an account is a row
-- insert, not a migration. See ARCHITECTURE.md §4.6 / BACKEND.md §6.

-- citext gives case-insensitive email columns (equality/uniqueness ignore case).
CREATE EXTENSION IF NOT EXISTS citext;
-- btree_gist provides gist opclasses for scalar equality, required by the
-- verified_email_one_user exclusion constraint on auth_identities below.
CREATE EXTENSION IF NOT EXISTS btree_gist;

-- A TypeMore account. Auth-agnostic on purpose.
CREATE TABLE users (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- citext makes names case-insensitively unique ('Egor' == 'egor'). Bounds
    -- and charset are enforced here as the last line of defense; the service
    -- validates first and returns name_taken / bad_request.
    display_name citext      NOT NULL UNIQUE
        CHECK (char_length(display_name) BETWEEN 3 AND 20 AND display_name ~ '^[a-zA-Z0-9_.-]+$'),
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- One row per way to sign in to an account. provider='email' uses the email
-- address itself as provider_subject; OAuth providers use their stable user id.
-- unique(provider, provider_subject) is what prevents duplicate identities.
CREATE TABLE auth_identities (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider         text        NOT NULL CHECK (provider IN ('github', 'google', 'email')),
    provider_subject text        NOT NULL,
    email            citext,
    email_verified   boolean     NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_subject),
    -- provider='email' stores the address itself as provider_subject, which is
    -- plain text while email is citext. Require it lower-cased so 'Foo@x.com'
    -- and 'foo@x.com' cannot slip past the UNIQUE above as two identities.
    CONSTRAINT email_subject_lowercase
        CHECK (provider <> 'email' OR provider_subject = lower(provider_subject))
);
CREATE INDEX auth_identities_user_id_idx ON auth_identities (user_id);
-- Supports the no-auto-link collision check: "is there a VERIFIED identity with
-- this email?" Partial index keeps it small and matches the query's predicate.
CREATE INDEX auth_identities_verified_email_idx ON auth_identities (email) WHERE email_verified;
-- Schema-level no-auto-link guarantee: an email may be VERIFIED for at most one
-- user, enforced atomically by the index so two concurrent requests cannot both
-- pass the application's "already verified elsewhere?" lookup. user_id WITH <>
-- deliberately lets ONE user hold several verified identities with the same
-- email (email identity + linked GitHub). citext has no gist opclass, so the
-- constraint compares lower(email::text), which matches citext equality.
ALTER TABLE auth_identities ADD CONSTRAINT verified_email_one_user
    EXCLUDE USING gist ((lower(email::text)) WITH =, user_id WITH <>)
    WHERE (email_verified);

-- Password material for email-provider accounts. Separate table so accounts that
-- only use OAuth carry no credential row at all.
CREATE TABLE user_credentials (
    user_id        uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    argon2id_hash  text        NOT NULL,
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- Opaque server-side sessions. Only the SHA-256 hash of the token is stored, so
-- a database leak does not yield usable session tokens.
CREATE TABLE sessions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash   bytea       NOT NULL UNIQUE,
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

-- Single-use, hashed tokens for email verification and password reset. Same
-- machinery for both, distinguished by purpose.
CREATE TABLE email_tokens (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    purpose    text        NOT NULL CHECK (purpose IN ('verify', 'reset')),
    token_hash bytea       NOT NULL UNIQUE,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX email_tokens_user_id_idx ON email_tokens (user_id);

-- +goose Down
DROP TABLE email_tokens;
DROP TABLE sessions;
DROP TABLE user_credentials;
DROP TABLE auth_identities;
DROP TABLE users;
DROP EXTENSION IF EXISTS btree_gist;
DROP EXTENSION IF EXISTS citext;
