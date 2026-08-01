# Self-hosting TypeMore

The server builds, boots and works without the anti-cheat review policy. This
page is about what that instance does, what it does not, and how to tell.

---

## The short version

```sh
make build          # no review policy
./bin/typemore-server
```

The boot log says:

```
WARN review policy disabled: runs are judged for correctness only
     policyVersion=none rebuildWith="-tags anticheat"
     consequence="no suspicion is computed, no run reaches the review queue,
                  and this instance's leaderboards are not protected"
```

`GET /healthz` says the same thing, in JSON, forever:

```json
{
  "status": "ok",
  "reviewPolicy": {
    "enabled": false,
    "version": "none",
    "warning": "review policy disabled: runs are judged for correctness only. No suspicion is computed and no run reaches the review queue, so this instance's leaderboards are not protected. See docs/SELF_HOST.md."
  }
}
```

It returns **200**. An instance without anti-cheat is a deployment, not an
outage — returning 503 would pull a working server out of a load balancer to
make a point.

---

## What still works — which is nearly everything

Removing the policy removes **judgement**, not correctness. Every run is still
replayed in full, on the server, through the same core the browser ran:

- **Words regenerated from the seed.** The client sends a seed and a dictionary
  hash, never the text. The server regenerates the words itself, so a client that
  typed different text than it claimed is caught.
- **The log folded through the vendored core.** Not a Go reimplementation — the
  frontend's own `src/shared/core`, compiled and vendored (`internal/replay/corejs`).
  Whatever it computes *is* the answer.
- **Score and metrics recomputed and compared.** The score total is compared
  **exactly**; the metrics within `1e-9`. A disagreement flags the run.
- **Every hard refusal.** A structurally invalid log, a broken `seq` chain, a
  failed commit-consistency check, an unknown dictionary, an event count or body
  size over the caps, an unsupported log or score version. All of these are
  refused with no policy involved — proven, not asserted, by
  `TestHardVerdictsDoNotDependOnTheJudge`, which runs the whole tamper matrix
  against judges ranging from "review nothing, ever" to "review every flagged
  run" and gets identical verdicts.
- **Bans and moderation.** The admin surface (`/api/v1/admin`, enabled by
  setting `TYPEMORE_ADMINS`; docs/MODERATION.md), the restriction gate on
  submission, the filtered leaderboard view. All open, and deliberately so: an
  instance has to be able to remove someone.
- **Leaderboards, profiles, the keyboard heatmap, quotes, the replay worker and
  its queue.** Untouched.

A cheater who edits their log, forges a score, or replays someone else's run is
caught by an instance with no policy at all.

## What does not work

**Nothing computes suspicion, and nothing goes to review.**

The plausibility flags the core raises — `min-interval`, `zero-variance`,
`superhuman-burst`, `uniform-intervals`, `paste` and the rest — are still
detected, still stored on every run, and still visible in the `validation`
column. Nothing weighs them. No run is routed anywhere on account of them.

Say it plainly: **the leaderboards of an instance without a review policy are not
protected.** A run that is *structurally perfect but humanly impossible* — an
automation that produces a flawless log with machine-uniform keystroke timing —
is accepted. The detectors see it. Nothing acts on it.

That is a real gap, not a theoretical one, and whether it matters depends
entirely on who can submit runs to your instance. For a private server among
people who know each other, it is fine. For a public board with a signup form, it
is not.

## Turning it on

```sh
go build -tags anticheat ./cmd/server
```

The boot log flips to `INFO review policy enabled policyVersion=2`, `/healthz`
reports `"enabled": true`, and `TYPEMORE_REPLAY_FLAG_WEIGHTS` /
`TYPEMORE_REPLAY_REVIEW_THRESHOLD` become live tuning knobs.

Without the tag those variables tune nothing, and the server says so rather than
ignoring them:

```
WARN replay policy tuning is set but this build has no policy to tune
```

### Runs judged before you turned it on

They are marked, permanently and automatically. Every decision records the
judge's version, and a run judged with no policy stores a **NULL**
`policy_version` and carries no `policy` block in its `validation` document — not
a block full of zeroes, which would read as "a policy looked and found nothing".

`revalidate` claims runs whose `policy_version IS NULL`, so:

```sh
go run -tags anticheat ./cmd/replayctl revalidate
```

re-judges your entire history through the policy you just enabled. Nothing has to
be remembered and nothing is lost — which is the whole reason the version is
written on every run rather than only when there is one.

`replayctl` built WITHOUT the tag refuses both of its subcommands, rather than
appearing to work: `calibrate` would print a page of zeroes, and `revalidate`
would rewrite every stored verdict as unjudged.

---

## Why it is a build tag and not a setting

A runtime switch — an env var, a config field — leaves the weights, the
thresholds and the rule names inside the binary whether the switch is on or off.
`strings ./typemore-server | grep bot_cadence` and the answer key is on your
screen. That is not a hidden policy, it is an inconvenienced one.

A build tag means the code is not compiled in. `TestBinaryWithoutTheTagCarriesNoPolicy`
builds the server both ways and greps the bytes: the untagged binary must not
contain the rule ids or the weights-table symbols, and — the control that makes
that assertion mean something — the tagged binary must.

What is **not** hidden, in either build: the detectors. They live in the core,
which runs in your browser, and which is vendored into this repository in
readable form. Hiding them would be pretending. What is behind the tag is only
what the server *does* about what they report — the weights, the review
threshold, the combination rules, and the version those rules are known by.

## The pattern

This is the same shape the captcha already uses: a nil verifier is a no-op
(`internal/auth/service.go`), the instance boots without one, and the endpoints
it would have guarded still work. Optional infrastructure degrades to a documented
no-op instead of a startup failure or a silent partial.

The difference is loudness. A missing captcha is visible the first time somebody
looks at a signup form. A missing review policy is invisible by construction —
so it warns at startup, and it keeps saying so on `/healthz` for as long as the
instance runs.
