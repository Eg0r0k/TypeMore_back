-- Queries for the auth domain. sqlc generates type-safe Go from these into
-- internal/auth/authdb. Transactions that span several of them (register, oauth
-- create) are composed in the Postgres store adapter, not here.

-- name: CreateUser :one
INSERT INTO users (display_name)
VALUES ($1)
RETURNING *;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: UpdateUserSettings :one
-- The two privacy switches, replaced as a pair. The handler resolves a partial
-- PATCH against the current row before calling this, so the statement itself
-- stays a plain write with no COALESCE cleverness to test.
--
-- updated_at is maintained HERE, by the writer, not by a trigger (00022): any
-- future statement that mutates users must set it the same way.
UPDATE users
SET profile_public = $2, keyboard_public = $3, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteUser :exec
DELETE FROM users WHERE id = $1;

-- name: PromoteAdmins :execrows
-- The admin bootstrap (00023, docs/MODERATION.md): accounts owning a VERIFIED
-- identity on one of the configured emails are promoted at startup. Verified
-- only — an address someone merely typed must not confer a role. Promotion
-- only, never demotion: the env list is how the first admin appears, not a
-- sync source. Idempotent by the role guard, and the guard is also what keeps
-- updated_at honest — an already-admin row is not touched.
UPDATE users
SET role = 'admin', updated_at = now()
WHERE role <> 'admin'
  AND id IN (SELECT user_id
             FROM auth_identities
             WHERE email_verified
               AND email = ANY (@emails::citext[]));

-- name: CreateIdentity :one
INSERT INTO auth_identities (user_id, provider, provider_subject, email, email_verified)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetIdentityByProviderSubject :one
SELECT * FROM auth_identities
WHERE provider = $1 AND provider_subject = $2;

-- name: GetEmailIdentityByEmail :one
SELECT * FROM auth_identities
WHERE provider = 'email' AND email = $1;

-- name: GetVerifiedIdentityByEmail :one
-- Collision check for the no-auto-link policy: any VERIFIED identity (from any
-- provider) already owning this email.
SELECT * FROM auth_identities
WHERE email = $1 AND email_verified
LIMIT 1;

-- name: ListIdentitiesByUser :many
SELECT * FROM auth_identities
WHERE user_id = $1
ORDER BY created_at;

-- name: SetIdentityEmailVerified :exec
UPDATE auth_identities SET email_verified = true WHERE id = $1;

-- name: VerifyEmailIdentityByUser :exec
UPDATE auth_identities SET email_verified = true WHERE user_id = $1 AND provider = 'email';

-- name: UpsertCredential :exec
INSERT INTO user_credentials (user_id, argon2id_hash)
VALUES ($1, $2)
ON CONFLICT (user_id)
DO UPDATE SET argon2id_hash = EXCLUDED.argon2id_hash, updated_at = now();

-- name: GetCredentialByUser :one
SELECT * FROM user_credentials WHERE user_id = $1;

-- name: CreateSession :one
INSERT INTO sessions (token_hash, user_id, expires_at)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetSessionByTokenHash :one
SELECT * FROM sessions WHERE token_hash = $1;

-- name: TouchSession :exec
UPDATE sessions SET last_seen_at = now(), expires_at = $2 WHERE id = $1;

-- name: DeleteSessionByTokenHash :exec
DELETE FROM sessions WHERE token_hash = $1;

-- name: DeleteUserSessions :exec
DELETE FROM sessions WHERE user_id = $1;

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE expires_at < now();

-- name: CreateEmailToken :one
INSERT INTO email_tokens (user_id, purpose, token_hash, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: DeleteUserTokensByPurpose :exec
DELETE FROM email_tokens WHERE user_id = $1 AND purpose = $2;

-- name: DeleteStaleEmailTokens :execrows
-- Janitor sweep: tokens that expired, or were consumed, more than 24 hours ago.
-- Recently expired/used rows are kept briefly for debugging; UseEmailToken
-- already refuses them regardless.
DELETE FROM email_tokens
WHERE expires_at < now() - interval '24 hours'
   OR used_at   < now() - interval '24 hours';

-- name: UseEmailToken :one
-- Atomically consume a token: marks it used only if unused, unexpired, and of
-- the right purpose. A returned row means the token was valid and is now spent.
UPDATE email_tokens
SET used_at = now()
WHERE token_hash = $1
  AND purpose = $2
  AND used_at IS NULL
  AND expires_at > now()
RETURNING *;
