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

`register`, `verify/resend` and `password-reset/request` additionally accept a
`turnstileToken` string in their JSON body and are gated on it when a captcha
secret is configured — see [Captcha](#captcha-cloudflare-turnstile). No other
endpoint takes the field.

### Success bodies

All success responses are `200 OK` with `Content-Type: application/json`
(OAuth `start`/`callback` are `302` redirects with no body). The two user-object
endpoints share one shape — the `userView` `{id, displayName, createdAt}`,
lower-camelCase, `createdAt` an RFC 3339 string:

| Endpoint | Body |
|---|---|
| `GET /me` | `{"id":"<uuid>","displayName":"<name>","createdAt":"<rfc3339>"}` |
| `POST /auth/login` | same `userView` object as `/me` |
| `POST /auth/logout` | `{"status":"ok"}` |
| `POST /auth/register` | `{"status":"ok","message":"if that email can receive mail, a message is on its way"}` |
| `POST /auth/verify/resend` | same generic `{status, message}` as `register` |
| `POST /auth/password-reset/request` | same generic `{status, message}` as `register` |
| `POST /auth/email/add` | same generic `{status, message}` as `register` |
| `POST /auth/verify` | `{"status":"ok","message":"email verified; you can now sign in"}` |
| `POST /auth/password-reset/confirm` | `{"status":"ok","message":"password updated; sign in with your new password"}` |
| `POST /auth/password/set` | `{"status":"ok","message":"password set; you can now sign in with email and password"}` |
| `POST /auth/link/{provider}/start` | `{"authorizeUrl":"<provider authorize URL>"}` |

`login` deliberately returns the full user object (not an empty body); the
session is delivered in the `Set-Cookie` header either way, so a client may read
the user from the login response or re-fetch `GET /me`. The generic
`{status, message}` bodies are identical across success and anti-enumeration
paths (see the security posture below). `/me` field names are pinned by a JSON
contract test (`TestMeJSONContract`), since the client parses them strictly.

### Error codes

`bad_request`, `invalid_token`, `invalid_credentials`, `email_not_verified`,
`name_taken`, `rate_limited`, `captcha_required`, `captcha_failed`,
`unauthorized`, `forbidden_origin`, `unknown_provider`, `internal`. `register`
returns `name_taken` (409) when the display name is already in use
(case-insensitively). `verify` and `password-reset/confirm` return
`account_exists_use_linking` (409) when the
email got verified by another account in the meantime. OAuth failures are
delivered as `?error=` on the frontend redirect: `invalid_state`,
`oauth_denied`, `oauth_exchange_failed`, `oauth_userinfo_failed`,
`account_exists_use_linking`, `provider_already_linked`. The account addendum
adds `email_already_set` (409, the account already has an email identity),
`no_verified_email` (409, `password/set` before an email is added and verified),
and `password_already_set` (409, `password/set` when a credential already
exists — changing a password stays under the reset flow). `captcha_required`
(400) and `captcha_failed` (400) are returned only by the three captcha-gated
endpoints, and only when a captcha secret is configured.

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
- **Hashing is gated.** Those parameters cost ~19 MiB and ~25 ms per hash, paid
  on the request path *before* anything can reject the caller and on every
  login whether the account exists or not. Unbounded, that is a memory-exhaustion
  DoS made of ordinary login attempts — measured at **1.4–2.5 GiB for 200
  concurrent unauthenticated requests** — and the per-IP limiter cannot stop it,
  because a distributed caller never trips it and the memory is committed by the
  hash it has already let through. A counting semaphore
  (`TYPEMORE_AUTH_HASH_CONCURRENCY`, sized from the detected memory ceiling)
  bounds concurrent hashes; a request that cannot get a slot within
  `TYPEMORE_AUTH_HASH_WAIT` is shed with **503 `overloaded`**. Shedding is
  uniform — a saturated server rejects a valid login and an invalid one
  identically, before either touches a hash, so it opens no enumeration oracle.
  Numbers, and why the gate costs no throughput, in
  [`PERFORMANCE.md`](PERFORMANCE.md) zone 1.
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
- **Captcha on the mailing endpoints.** `register`, `verify/resend` and
  `password-reset/request` sit behind a Cloudflare Turnstile gate that runs
  *before* the per-IP limiter, so an unproven caller is turned away with
  `captcha_required` instead of quietly draining the bucket shared by everyone
  behind that NAT. Disabled by default (empty secret). Details below.
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
| `TYPEMORE_TURNSTILE_SECRET` | *(empty)* | Turnstile secret key; **empty disables captcha entirely** |
| `TYPEMORE_AUTH_CLEANUP_INTERVAL` | `1h` | Janitor sweep interval (≤0 disables) |
| `TYPEMORE_AUTH_HASH_CONCURRENCY` | *(derived)* | Max concurrent argon2id hashes; 0 derives from the memory budget |
| `TYPEMORE_AUTH_HASH_MEMORY_BUDGET` | *(derived)* | Peak bytes hashing may hold; 0 derives from the detected memory ceiling (¼ of it), else 512 MiB |
| `TYPEMORE_AUTH_HASH_WAIT` | `500ms` | How long a request queues for a hashing slot before a 503 |

## Configuring OAuth apps

**GitHub:** https://github.com/settings/developers → New OAuth App. Authorization
callback URL: `<OAUTH_REDIRECT_BASE>/api/v1/auth/oauth/github/callback`. Put the
Client ID/secret in `TYPEMORE_GITHUB_CLIENT_ID`/`_SECRET`.

**Google:** https://console.cloud.google.com/apis/credentials → Create OAuth
client ID (Web application). Authorized redirect URI:
`<OAUTH_REDIRECT_BASE>/api/v1/auth/oauth/google/callback`. Put the Client
ID/secret in `TYPEMORE_GOOGLE_CLIENT_ID`/`_SECRET`.

## Captcha (Cloudflare Turnstile)

Three endpoints send mail to an address supplied by an unauthenticated caller:
`POST /auth/register`, `POST /auth/verify/resend` and
`POST /auth/password-reset/request`. That is the whole abuse surface —
everything else either needs a session or consumes a token we minted. Those
three, and only those three, accept `turnstileToken` (a string) in their JSON
body.

**Empty `TYPEMORE_TURNSTILE_SECRET` disables the captcha entirely**, and that is
the default. A captcha is a dependency on a third party; requiring one would
mean `make run` and the test suite could not create an account without a
Cloudflare account, a site key pasted into the frontend, and an outbound network
path. With the secret unset the three endpoints behave exactly as they did
before the captcha existed: no token is required, and one sent anyway is
ignored rather than rejected. The frontend mirrors this — absent
`VITE_TURNSTILE_SITE_KEY`, no widget is rendered and no token is sent.

### Behaviour when enabled

| Situation | Response |
|---|---|
| Token missing, empty, or whitespace | `400 captcha_required` |
| Body unparseable (hence no token) | `400 captcha_required` |
| siteverify answered `success: false` | `400 captcha_failed` |
| siteverify unreachable, timed out, or answered non-200/garbage | `400 captcha_failed` |

A provider outage is deliberately **not** a 500. The server is healthy; the
request simply cannot be proven, and the client's remedy — solve a fresh
challenge and retry — is the same as for a rejected token. Collapsing the two
also keeps Cloudflare's `error-codes` (`invalid-input-response`,
`timeout-or-duplicate`, `invalid-input-secret`, …) out of the response: they are
logged for the operator and never returned, so a prober cannot use the gate to
learn how it is configured.

### Where the gate sits

The gate is the first middleware on those three routes: **captcha → per-IP rate
limiter → Origin/CSRF check → handler**. Two consequences worth stating
explicitly:

- A caller without a token never spends a rate-limit token, so a token-less
  flood cannot exhaust the bucket of a legitimate user sharing its IP.
- Once the gate passes, nothing downstream changes. The anti-enumeration
  responses are byte-identical to the pre-captcha ones — a taken-email register
  and an unknown-email reset still return the same generic
  `{"status":"ok","message":"if that email can receive mail, …"}` as the happy
  path. The gate may refuse a caller; it must never become an existence oracle.

The client IP sent to siteverify as `remoteip` is derived exactly as the rate
limiter derives its key (`clientIP`, the transport `RemoteAddr`), so there is
one notion of "who is calling" to fix when a reverse proxy arrives. It is
omitted when it does not parse as an IP address.

### Layering

`auth.CaptchaVerifier` is consumer-declared in `internal/auth` next to
`auth.Mailer`; the implementation is `internal/platform/turnstile`, and
`cmd/server` wires them (BACKEND.md §2: platform imports no domain). Unlike the
mailer no adapter is needed — the platform signature carries no domain type.
`turnstile.New` returns `nil` for an empty secret, so "is the captcha on?" is
answered once, at construction. The composition root converts that typed nil
into a nil *interface* (`newCaptchaVerifier`); handing back the typed nil
directly would make the domain's "nil means disabled" test pass while the value
is unusable, which is why that bridge has a test of its own.

### Local development: Cloudflare's test keys

Cloudflare publishes dummy keys that always produce a fixed verdict. They work
from any domain including `localhost`, a test secret key accepts **only** the
dummy token, and a production secret key rejects it — so the pair must be used
together. Values below are from Cloudflare's own documentation
(<https://developers.cloudflare.com/turnstile/troubleshooting/testing/>);
re-check there if a key ever stops behaving as described.

Site keys (frontend, `VITE_TURNSTILE_SITE_KEY`):

| Site key | Behaviour |
|---|---|
| `1x00000000000000000000AA` | Always passes (visible) |
| `2x00000000000000000000AB` | Always blocks (visible) |
| `1x00000000000000000000BB` | Always passes (invisible) |
| `2x00000000000000000000BB` | Always blocks (invisible) |
| `3x00000000000000000000FF` | Forces an interactive challenge (visible) |

Secret keys (backend, `TYPEMORE_TURNSTILE_SECRET`):

| Secret key | Behaviour |
|---|---|
| `1x0000000000000000000000000000000AA` | Always passes |
| `2x0000000000000000000000000000000AA` | Always fails |
| `3x0000000000000000000000000000000AA` | Yields a "token already spent" error |

To exercise the happy path end to end, pair site key
`1x00000000000000000000AA` with secret `1x0000000000000000000000000000000AA`.
To see `captcha_failed` without unplugging the network, keep the passing site
key and switch the secret to `2x0000000000000000000000000000000AA`. Never set a
dummy secret in production: it accepts any token as valid.

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
