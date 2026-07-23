# TypeMore Auth & Persistence

Phase 2 of the server: PostgreSQL persistence plus authentication
(email/password + GitHub/Google OAuth), opaque server-side sessions, email
verification, and password reset. This document is the reference for the auth
surface; the realtime protocol lives in [`PROTOCOL.md`](PROTOCOL.md).

## Endpoints

All under `/api/v1`. Mutating (POST) endpoints require an `Origin` header equal
to `TYPEMORE_FRONTEND_ORIGIN` (CSRF defense) and are rate-limited per client IP.
Success/JSON bodies are shown; errors are `{"error":"<code>","message":"..."}`.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET  | `/healthz` | — | Liveness + build info |
| GET  | `/readyz` | — | Readiness (DB ping) |
| POST | `/api/v1/auth/register` | — | Create unverified email account, send verification link |
| POST | `/api/v1/auth/verify` | — | Consume verification token (single-use, 24h) |
| POST | `/api/v1/auth/verify/resend` | — | Re-send verification link |
| POST | `/api/v1/auth/login` | — | Email+password → session cookie |
| POST | `/api/v1/auth/logout` | session | Delete current session |
| POST | `/api/v1/auth/password-reset/request` | — | Send reset link (1h) |
| POST | `/api/v1/auth/password-reset/confirm` | — | Set new password, **revoke all sessions** |
| GET  | `/api/v1/auth/oauth/{provider}/start` | — | Begin OAuth (redirect to provider) |
| GET  | `/api/v1/auth/oauth/{provider}/callback` | — | Complete OAuth → session, redirect to frontend |
| POST | `/api/v1/auth/link/{provider}/start` | session | Begin linking a provider to the current account (returns `{authorizeUrl}`) |
| POST | `/api/v1/auth/email/add` | session | Add an email identity to an OAuth-only account, send verification |
| POST | `/api/v1/auth/password/set` | session | One-time first password for an account with a verified email and no credentials |
| GET  | `/api/v1/me` | session | Current user |

`{provider}` is `github` or `google`. OAuth callbacks redirect to
`<frontend>/auth/callback?status=ok` (or `?error=<code>`, e.g.
`account_exists_use_linking`), and linking to `?linked=<provider>`.

### Error codes

`bad_request`, `invalid_token`, `invalid_credentials`, `email_not_verified`,
`name_taken`, `rate_limited`, `unauthorized`, `forbidden_origin`,
`unknown_provider`, `internal`. `register` returns `name_taken` (409) when the
display name is already in use (case-insensitively). `verify` and
`password-reset/confirm` return `account_exists_use_linking` (409) when the
email got verified by another account in the meantime. OAuth failures are
delivered as `?error=` on the frontend redirect: `invalid_state`,
`oauth_denied`, `oauth_exchange_failed`, `oauth_userinfo_failed`,
`account_exists_use_linking`, `provider_already_linked`. The account addendum
adds `email_already_set` (409, the account already has an email identity),
`no_verified_email` (409, `password/set` before an email is added and verified),
and `password_already_set` (409, `password/set` when a credential already
exists — changing a password stays under the reset flow).

### Display names

3–20 characters of `[a-zA-Z0-9_.-]`, unique **case-insensitively** (`Egor` and
`egor` are the same name; citext UNIQUE + CHECK in the schema). Register
rejects a taken name with `name_taken`; an omitted name defaults to the email
local-part (sanitized). OAuth account creation derives the name from the
provider profile (profile name, else email local-part, else `player`,
sanitized to the charset) and resolves collisions with a numeric suffix:
`name`, `name1`, `name2`, …

## Adding an email / setting a password (OAuth-only accounts)

An account created via OAuth may have no email identity and no password. Two
session-authenticated endpoints let it grow email+password login:

- `POST /email/add {email}` creates an **unverified** email identity and sends a
  verification link through the existing token machinery. The regular
  `POST /verify` then flips it to verified (same path as registration). If the
  address already has an email identity, or is verified under another account,
  the response is `account_exists_use_linking` (the unique `provider/subject`
  constraint and, at verify time, the `verified_email_one_user` exclusion). An
  account that already has an email identity gets `email_already_set`.
- `POST /password/set {password}` sets a **first** password, allowed only when
  the account has a **verified email identity** and **no** credential row yet
  (`no_verified_email` / `password_already_set` otherwise). Changing an existing
  password stays under the reset flow. After this, email+password login works.

Both reuse the auth rate limiter and Origin/CSRF checks. The verify step is the
anti-enumeration boundary: the address owner must control the mailbox to
complete it, and the collision constraints are the atomic backstop.

## Schema

```
users
  id            uuid pk
  display_name  citext UNIQUE            (3–20 chars, ^[a-zA-Z0-9_.-]+$ CHECK)
  created_at    timestamptz
      │ 1
      │
      ├──< auth_identities            (how you sign in; unique(provider,provider_subject))
      │      id, user_id fk→users ON DELETE CASCADE
      │      provider  ∈ {github,google,email}
      │      provider_subject text    (email addr for provider='email';
      │                                CHECK lower-cased for that provider)
      │      email citext, email_verified bool
      │      created_at
      │      idx(user_id); partial idx(email) WHERE email_verified
      │      EXCLUDE (lower(email) =, user_id <>) WHERE email_verified
      │        — one email verified for at most ONE user (btree_gist)
      │
      ├──1  user_credentials          (only for email accounts)
      │      user_id pk fk→users ON DELETE CASCADE
      │      argon2id_hash text, updated_at
      │
      ├──< sessions                    (opaque; only the hash is stored)
      │      id, token_hash bytea UNIQUE, user_id fk→users ON DELETE CASCADE
      │      created_at, expires_at, last_seen_at
      │      idx(user_id), idx(expires_at)
      │
      └──< email_tokens                (single-use verify/reset)
             id, user_id fk→users ON DELETE CASCADE
             purpose ∈ {verify,reset}, token_hash bytea UNIQUE
             expires_at, used_at, created_at
             idx(user_id)
```

Everything cascades from `users`, so account deletion (BACKEND.md §8) is a single
`DELETE FROM users`.

## Security posture

- **Passwords:** argon2id (OWASP 2024 params: 19 MiB / t=2 / p=1), parameters
  encoded in each PHC hash so they can be raised without breaking old hashes.
- **Sessions:** 256-bit opaque tokens; only the SHA-256 hash is stored. Cookie is
  `HttpOnly`, `SameSite=Lax`, `Secure` (config), 30-day sliding expiry.
- **CSRF:** mutating endpoints require `Origin == FRONTEND_ORIGIN` (defense in
  depth with `SameSite=Lax` and CORS credentials scoped to the frontend).
- **OAuth:** authorization-code flow with a state cookie **and** PKCE (S256).
- **No auto-link:** an OAuth login whose verified email matches an existing
  verified account is rejected with `account_exists_use_linking` — the user must
  sign in and link explicitly. Prevents account takeover via a weak provider.
  Enforced twice: an application lookup for the friendly redirect, and the
  `verified_email_one_user` exclusion constraint as the atomic backstop, so two
  concurrent requests cannot leave one email verified under two users. The same
  user may hold several verified identities with one email (email + linked
  GitHub).
- **Anti-enumeration:** register (taken email) and reset/resend (unknown email)
  return the *same* success as the happy path; the password hash is computed
  regardless (including a decoy hash on unknown-email login) to keep timing
  uniform.
- **Password reset revokes all sessions** of the user.
- Tokens, passwords, and hashes are never logged.
- **Expiry janitor:** a background goroutine (started in `cmd/server`, stopped
  by the shutdown context) deletes expired sessions and email tokens that
  expired or were used more than 24 hours ago, every
  `TYPEMORE_AUTH_CLEANUP_INTERVAL` (default hourly), logging per-sweep counts.
  Expiry is still enforced at read time; the janitor is hygiene only.

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `TYPEMORE_DATABASE_URL` | `postgres://typemore:typemore@localhost:5432/typemore?sslmode=disable` | pgx DSN |
| `TYPEMORE_DB_MAX_CONNS` | `10` | Pool size |
| `TYPEMORE_FRONTEND_ORIGIN` | `http://localhost:5173` | CORS origin, required `Origin`, email link base |
| `TYPEMORE_COOKIE_NAME` | `tm_session` | Session cookie name |
| `TYPEMORE_COOKIE_DOMAIN` | *(empty)* | Cookie domain (host-only if empty) |
| `TYPEMORE_COOKIE_SECURE` | `true` | Cookie `Secure` (set false for HTTP dev) |
| `TYPEMORE_SESSION_TTL` | `720h` | Sliding session lifetime |
| `TYPEMORE_GITHUB_CLIENT_ID` / `_SECRET` | *(empty)* | GitHub OAuth app; empty disables |
| `TYPEMORE_GOOGLE_CLIENT_ID` / `_SECRET` | *(empty)* | Google OAuth app; empty disables |
| `TYPEMORE_OAUTH_REDIRECT_BASE` | `http://localhost:8080` | This server's public base URL for callbacks |
| `TYPEMORE_SMTP_HOST` | *(empty)* | SMTP host; empty logs the link instead of sending |
| `TYPEMORE_SMTP_PORT` | `1025` | SMTP port (Mailpit) |
| `TYPEMORE_SMTP_USERNAME` / `_PASSWORD` | *(empty)* | SMTP AUTH (omit for Mailpit) |
| `TYPEMORE_SMTP_FROM` | `no-reply@typemore.local` | Envelope/From |
| `TYPEMORE_AUTH_RATE_EVERY` | `1s` | Token-bucket refill interval |
| `TYPEMORE_AUTH_RATE_BURST` | `10` | Token-bucket size (per IP) |
| `TYPEMORE_AUTH_CLEANUP_INTERVAL` | `1h` | Janitor sweep interval (≤0 disables) |

## Configuring OAuth apps

**GitHub:** https://github.com/settings/developers → New OAuth App. Authorization
callback URL: `<OAUTH_REDIRECT_BASE>/api/v1/auth/oauth/github/callback`. Put the
Client ID/secret in `TYPEMORE_GITHUB_CLIENT_ID`/`_SECRET`.

**Google:** https://console.cloud.google.com/apis/credentials → Create OAuth
client ID (Web application). Authorized redirect URI:
`<OAUTH_REDIRECT_BASE>/api/v1/auth/oauth/google/callback`. Put the Client
ID/secret in `TYPEMORE_GOOGLE_CLIENT_ID`/`_SECRET`.

## Deliberate deviations from BACKEND.md

- **Sessions in Postgres, not Redis.** One store is less operational surface than
  standing up Redis for a single table; Redis arrives with leaderboards where it
  is genuinely needed. Session storage is behind the `SessionStore` interface
  (`internal/auth`), so the swap is: implement `SessionStore` against Redis and
  change one line in `cmd/server`.
- **Concrete mailers live in `internal/platform/mail`, adapted to the
  `auth.Mailer` interface at the composition root.** BACKEND.md §2 forbids
  `platform` importing a domain, so the platform senders use a neutral `Message`
  type and `cmd/server` bridges them to `auth.Mail` — keeping the Mailer
  interface consumer-declared in `auth` while the impls stay in `platform`.

## Open questions

- **Account deletion endpoint.** The schema supports cascade delete (BACKEND.md
  §8), but no HTTP endpoint exists yet — deferred with the admin surface.
- **Reverse-proxy client IP.** Rate limiting keys on `RemoteAddr`. Behind a proxy
  a trusted `X-Forwarded-For` parse is needed (single-binary assumption for now).
- **Real-provider PKCE.** PKCE is exercised end-to-end against the test provider
  and sent to real providers; GitHub's PKCE support for OAuth apps should be
  confirmed when its app is created (state remains the primary CSRF defense).
- **OAuth account with no email — resolved.** A provider that withholds email
  now creates an account without one; the owner can attach and verify an email
  (`POST /email/add`) and set a first password (`POST /password/set`). See
  "Adding an email / setting a password" above.
