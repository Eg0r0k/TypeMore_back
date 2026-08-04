# Deploy backlog

Work that is deliberately NOT done yet because it cannot be done correctly
before the server sits behind its real reverse proxy. Each item says what
breaks if it ships early, so the decision is a record rather than a memory.

## 1. Trusted client IP (`X-Forwarded-For`) — and everything keyed on it

**State today.** `httpx.ClientIP` derives the key from `RemoteAddr` only. No
forwarded header is read, and that is correct for a server reachable directly:
`X-Forwarded-For` is written by the client, so a server that trusted it without
a proxy in front would be trusting the attacker's own string.

**What is blocked on this.**

- **A cap on rooms per IP for guests.** A guest opens N tabs and creates N
  rooms; one connection is one seat, but nothing bounds how many connections an
  address may hold. The obvious answer — 3 open rooms per IP — is unshippable
  today in both directions:
  - behind a reverse proxy, `RemoteAddr` is the PROXY's address, so "3 per IP"
    becomes "3 open rooms in the entire product" and the first three guests
    lock everyone else out;
  - reading `X-Forwarded-For` without a trusted-proxy list means the abuser
    rotates a header string per request while the honest client, which sends no
    such header, is the only one the cap can still reach.

  So the code would be written now, not work now, and be rewritten at deploy.

- **Every other per-IP limiter's accuracy** — auth, lobby list, public replay,
  profile search, the board index. They all work today (RemoteAddr is the real
  client) and all silently degrade to "per proxy" the moment a proxy appears.
  This item is what keeps that from being a surprise.

**When it is done, check the number against CGNAT.** Mobile carriers put
thousands of subscribers behind one address, and university and office NATs are
not far behind. A room cap of 3 per IP would be indistinguishable from an
outage for a phone network. Either the cap is generous enough to be useless
against a determined abuser (in which case say so and drop it), or it is keyed
on something better than an address — an account, with guests bounded some
other way.

**Definition of done.** A trusted-proxy configuration (`TYPEMORE_TRUSTED_PROXIES`
or equivalent), `httpx.ClientIP` reading the last untrusted hop, one test that a
spoofed header from an untrusted source is ignored, and only then the room cap
with a number chosen against real CGNAT data.
