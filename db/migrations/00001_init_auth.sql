-- +goose Up
-- Auth + identity schema. Users are separate from the credentials/identities
-- that authenticate them, so linking a second provider to an account is a row
-- insert, not a migration. See ARCHITECTURE.md §4.6 / BACKEND.md §6.

-- citext gives case-insensitive email columns (equality/uniqueness ignore case).
CREATE EXTENSION IF NOT EXISTS citext;

-- A TypeMore account. Auth-agnostic on purpose.
CREATE TABLE users (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    display_name text        NOT NULL,
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
    UNIQUE (provider, provider_subject)
);
CREATE INDEX auth_identities_user_id_idx ON auth_identities (user_id);
-- Supports the no-auto-link collision check: "is there a VERIFIED identity with
-- this email?" Partial index keeps it small and matches the query's predicate.
CREATE INDEX auth_identities_verified_email_idx ON auth_identities (email) WHERE email_verified;

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
DROP EXTENSION IF EXISTS citext;
