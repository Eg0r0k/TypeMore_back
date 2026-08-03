# Runbook — core release v4 (tokenizer, three timeline series, speed detector)

One deploy, one revalidate pass. Run it in this order; the ordering is the
load-bearing part, not the commands.

**Who runs this:** an operator with database and deploy access. The agent work is
finished — everything below is human-triggered, and every step says what success
looks like and what stops the release.

**What is in it**

| | change | who notices |
|---|---|---|
| A | `…` is rewritten to `...` in the tokenizer; `«»` join the double-quote equivalence group | players on 11 dictionaries and 34 quotes that were previously unwinnable |
| B | the results chart gains a cumulative `raw` series; `TimelinePoint.raw` → `burst` | every results screen |
| C | `superhuman-burst` loses its `accuracy === 1` gate and gains a duration-dependent ceiling | the review queue |
| D | `policy_version` 3 → 4 | `revalidate` |

**The one thing that must not be got wrong:** the backend deploys BEFORE the
frontend. The client sends its own numbers with the run and the server refuses a
run whose numbers it cannot reproduce, so a new client against an old server
means every run comes back `metric_mismatch`. An old client against a new server
is harmless — the server recomputes everything anyway.

---

## 0. Before you start — the forecast to check against

> **THE TABLE BELOW IS STALE AND MUST BE RE-MEASURED BEFORE STEP 1.**
>
> It was computed against a bundle that predates the in-flight WPM change (the
> word being typed is now worth its correct PREFIX rather than all-or-nothing).
> That moves `metrics.wpm` for every run that ended mid-word with an imperfect
> buffer — which in timed mode is most of them — so the "runs whose status
> changes" rows are no longer trustworthy in either direction.
>
> Re-measure by replaying the population fixture through the NEWLY VENDORED
> bundle:
>
> ```bash
> go test ./internal/replay/ -run TestSuperhumanBurst -v
> ```
>
> and by re-running the mismatch count against a copy of production. Do not
> deploy against the numbers below; they are kept only so the re-measurement has
> something to diff against.

These were computed from the export of 2026-08-03 (138 runs). Step 5 is expected
to reproduce them; a large divergence is itself a finding, so write down what you
actually get.

| | forecast |
|---|---|
| runs stored on one of the 11 `…` dictionaries | **0** |
| runs stored on one of the 34 `…` quotes | **0** |
| runs currently `flagged: metric_mismatch` | **41** — expected to become `accepted` |
| runs currently `flagged: score_mismatch` | **4** — expected to become `accepted`; if any survives, STOP and report |
| runs currently `flagged: bot_pattern` | 2 — expected to stay flagged |
| runs expected to GAIN `superhuman-burst` | **4**, all on `mental1sm` |
| runs expected to change accepted → flagged | **2** (336.2 wpm @ 99.6 % / 60 s, and 212.4 wpm @ 100 % / 30 s) |
| net status change | 45 flagged → accepted, 2 accepted → flagged |

The first two rows are why the tokenizer change is safe to ship without a
decision: **no stored run's target text moves.** If your production database is
not the one that export came from, re-run the count before step 2:

```sql
-- Runs on a dictionary whose word list contains U+2026.
-- The eleven hashes come from internal/replay/testdata/corpus-hashes/dictionaries.tsv
-- (belarusian_25k/50k/100k, tatar_crimean_5k/10k/15k,
--  tatar_crimean_cyrillic_5k/10k/15k, thai_50k/60k).
SELECT count(*) FROM runs WHERE dict_hash IN (
  '0b0e5701','14bd2ad1','282dda29','29193375','2c829cca','3031301d',
  '5349cb29','7bd6d37d','ce8b8a2a','f50df2a2','fe137bc0');

-- Runs on a quote whose text contains U+2026.
SELECT count(*) FROM runs r
  JOIN quotes q ON q.text_hash = r.setup->'generation'->'textSource'->>'quoteHash'
 WHERE q.text LIKE '%' || U&'\2026' || '%';
```

**If either count is non-zero, STOP.** Those runs were played against a target
that no longer exists, so a revalidate pass will re-judge them against different
words and some will fall out of `accepted`. Published bytes are frozen and the
decision about what happens to them is Egor's, not the migration's.

## 1. Back up the database

```bash
pg_dump --format=custom --file=typemore-pre-v4-$(date -u +%Y%m%dT%H%M%SZ).dump "$TYPEMORE_DATABASE_URL"
```

**Success:** the file exists and `pg_restore --list` reads it.
**Stop if:** it does not. Everything after this is reversible only from here —
step 5 overwrites the stored verdict of every run in the database.

## 2. Deploy the BACKEND

The binary must be built `-tags anticheat` or the policy is a `Noop` and step 5
judges nothing (it says so at startup and on `/healthz`).

```bash
make test-anticheat        # must be green before it ships
make build-anticheat
# ...deploy bin/typemore-server as you normally do...
```

**Success:** `/healthz` reports the new build, and its `bundleSha` is
`28e41f4ed5324a3c4654d360a614058a1fcc7cb9f1fa985103558a86998fda6c`.
**Stop if:** the sha is `1aba05e3…` — that is the previous bundle, and the
deploy did not take.

Between this step and step 3 the live client is the OLD one. That is the safe
direction and it is expected to last minutes, not hours.

## 3. Deploy the FRONTEND

```bash
cd ../TypeMore_front && pnpm build
# ...deploy dist/ as you normally do...
```

**Success:** a fresh run submitted from the deployed client comes back
`accepted`, not `metric_mismatch`.
**Stop if:** it comes back `metric_mismatch` — the two sides are running
different cores, which means one of the two deploys did not take. Do not
continue to step 5; a revalidate pass against a half-deployed pair rewrites
every verdict with the wrong core.

## 4. Set the canary epoch

Only if it has never been set. Until it is, the canary detectors are disarmed for
every run, which is correct and documented (`docs/REPLAY.md`, "Canary epoch") —
they must not judge runs whose client never rendered a canary.

```bash
# TYPEMORE_REPLAY_CANARY_EPOCH — the instant the canary-rendering client went
# live. Set it to the moment step 3 completed, never earlier.
```

**Success:** `/healthz` shows the epoch set.

## 5. Revalidate — one pass, everything at once

```bash
go run -tags anticheat ./cmd/replayctl revalidate
```

This single pass closes all of it: the stale-bundle `metric_mismatch` runs, the
`score_mismatch` runs, and — retroactively — the runs the repaired speed detector
should always have caught.

**Success:** the counts land near the step-0 forecast, and in particular the four
`score_mismatch` runs are gone.
**Stop and report if:**
- any `score_mismatch` survives — that is a real scoring divergence, not the
  stale bundle, and it needs its own investigation;
- more than a handful of runs move `accepted → flagged` beyond the two forecast.
  The detector is supposed to fire on four runs from one account and on nothing
  else in this population; a wider sweep means the ceiling table is wrong for
  your data, and the fix is `BURST_CEILING` in the core, not this pass.

## 6. Revalidate again — idempotency

```bash
go run -tags anticheat ./cmd/replayctl revalidate
```

**Success: 0 runs claimed.** The pass keys on `policy_version` and `bundle_sha`,
so a second run finds nothing left behind either.
**Stop if:** it claims runs again. Something is failing and rolling back
mid-transaction, and a loop that re-judges the same rows forever will do it
quietly.

## 7. Calibrate — the new baseline

```bash
go run -tags anticheat ./cmd/replayctl calibrate
```

Attach the whole output to the release notes. Every distribution recorded before
this deploy was measured through the broken detector, including the ones written
into `docs/REPLAY.md`, so this is the first baseline any future weight change may
be argued from.

**This is not a pass/fail step.** It is the input to the decision Egor asked to
keep for himself: whether the review threshold and the weights move next.

## 8. Verify the boards did not drift

```bash
make rebuild-leaderboards
```

**Success:** it reports *unchanged*.
**Stop if:** it does not. Step 5 moved runs into and out of `accepted`, and the
board projection is written inside the same transaction — so a rebuild that
disagrees means a run changed status without its projection following it.

---

## Rolling back

Steps 1–4 roll back by redeploying the previous binary and client. Step 5 does
not: it overwrites `validation`, `status`, `policy_version` and `bundle_sha` on
every run it touches, and the leaderboard projection follows it inside the same
transaction. **After step 5 the only rollback is the step-1 dump.** That is the
reason step 1 is a step and not a suggestion.
