-- +goose Up
--
-- The verdict moves off `runs` into a 1:1 satellite. 00004 put the replay
-- worker's output (server numbers, validation report, bundle_sha) directly on
-- the runs row, 00005 added policy_version, and by 00016 the table had begun
-- accreting one column per domain that wanted transaction locality with the
-- verdict. Each addition was individually justified — the verdict transaction
-- already holds the row lock, so a column there is atomicity for free — but
-- the sum is a table where ingestion facts, replay judgement and projection
-- bookkeeping interleave, every verdict column is nullable-by-phase, and the
-- invariant 00004 stated ("a decision writes all of them at once") is held by
-- convention in one UPDATE statement rather than by structure.
--
-- run_verdicts makes that invariant structural: a row EXISTS if and only if
-- the run has been judged, and inside the row nothing that a decision always
-- produces is nullable. `runs` goes back to being what 00002 declared it to
-- be — the immutable submission plus its lifecycle — and the replay domain
-- owns a table instead of a column subset.
--
-- What deliberately STAYS on runs, and why:
--
--   * status — the run's lifecycle, not the verdict's payload. The queue scan
--     (runs_pending_idx), the leaderboard eligibility view and every
--     accepted-only read across three domains key on it. The rejected
--     alternative — dropping status and treating "no verdict row" as pending —
--     turns the queue claim into an anti-join that cannot use a partial index
--     and repoints ~twenty accepted-filters for no gain in discipline: the
--     status CHECK plus the single writer (the worker) already constrain it.
--   * attempts / last_error — retry bookkeeping of the QUEUE, not judgement.
--     The pending claim reads attempts before any verdict row exists, so it
--     cannot live here.
--   * restarts_since_last_submit (00013) — client-reported ingestion data that
--     arrives WITH the submission. It looks projection-shaped but it is part
--     of what was submitted, like client_metrics.
--
-- The pair (bundle_sha, policy_version) stays nullable: rows judged before
-- 00004 recorded the bundle, or before the policy existed (00005's NULL
-- semantics), are history and history is not backfilled with invented values.
-- server_metrics/server_score stay nullable because an error verdict (replay
-- timeout, unknown dict) legitimately has no numbers. validation and
-- validated_at are NOT NULL: every decision since 00004 has written both, and
-- if some historical row violates that, this migration MUST fail loudly here
-- rather than silently mint a judged run with no verdict document.
-- user_id is a denormalized SNAPSHOT of runs.user_id — the same move
-- leaderboard_entries makes, for the same reason: one player's rows must be
-- reachable without touching anyone else's. The profile aggregates read a
-- whole account's server_metrics per request (docs/PERFORMANCE.md zone 9,
-- pinned plans), and without a player-scoped access path here the only join
-- strategies are a per-row primary-key probe or a hash of the entire verdict
-- table — the second of which the planner rightly picks at scale, and which
-- makes a profile page cost a function of EVERYONE's history. The copy cannot
-- drift: runs.user_id is immutable, and account deletion cascades through
-- both foreign keys to the same end.
CREATE TABLE run_verdicts (
    run_id         uuid        PRIMARY KEY REFERENCES runs (id) ON DELETE CASCADE,
    user_id        uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    server_metrics jsonb,
    server_score   jsonb,
    validation     jsonb       NOT NULL,
    bundle_sha     text,
    policy_version smallint,
    validated_at   timestamptz NOT NULL
);

CREATE INDEX run_verdicts_user_idx ON run_verdicts (user_id);

-- Backfill: every judged run gets its verdict row, columns copied verbatim.
-- The status <> 'pending' predicate is exactly the old runs_stale_policy_idx
-- partial-index predicate — the set this table now materializes.
INSERT INTO run_verdicts (run_id, user_id, server_metrics, server_score,
                          validation, bundle_sha, policy_version, validated_at)
SELECT id, user_id, server_metrics, server_score, validation,
       bundle_sha, policy_version, validated_at
FROM runs
WHERE status <> 'pending';

-- The eligibility view now reads the numbers from the satellite. Output
-- columns are IDENTICAL to 00017's definition — projection, rebuild and every
-- read still go through this one view (00009's rule) — only the FROM changes:
-- an inner join, because status = 'accepted' implies the verdict row exists.
-- The jsonb_typeof guards keep their original job (00006: a malformed verdict
-- document must not abort the batch that wrote it), now against v.*.
CREATE OR REPLACE VIEW leaderboard_eligible_runs AS
SELECT r.id      AS run_id,
       r.user_id AS user_id,
       r.mode,
       r.duration_ms,
       r.word_count,
       r.lang,
       run_text_source_kind(r.setup)              AS text_source_kind,
       q.id                                       AS quote_id,
       (v.server_score ->> 'total')::bigint       AS score,
       (v.server_metrics ->> 'wpm')::numeric      AS wpm,
       (v.server_metrics ->> 'raw')::numeric      AS raw,
       (v.server_metrics ->> 'accuracy')::numeric AS acc,
       run_mods(r.setup)                          AS mods,
       r.created_at                               AS achieved_at
FROM runs r
         JOIN run_verdicts v ON v.run_id = r.id
         LEFT JOIN LATERAL (SELECT run_quote_id(r.setup) AS quote_id) c ON true
         LEFT JOIN quotes q ON q.id = c.quote_id
WHERE r.status = 'accepted'
  AND run_adopted_from(r.setup) IS NULL
  AND CASE WHEN q.id IS NOT NULL
      THEN true
      ELSE run_text_source_kind(r.setup) = 'seeded'
           AND ((r.mode = 'time'  AND r.duration_ms IN (15000, 30000, 60000))
             OR (r.mode = 'words' AND r.word_count  IN (25, 50, 100)))
      END
  AND jsonb_typeof(v.server_score -> 'total')      = 'number'
  AND jsonb_typeof(v.server_metrics -> 'wpm')      = 'number'
  AND jsonb_typeof(v.server_metrics -> 'raw')      = 'number'
  AND jsonb_typeof(v.server_metrics -> 'accuracy') = 'number';

-- runs_stale_policy_idx is not replaced. Its partial predicate
-- (status <> 'pending') is now the run_verdicts table itself, and the
-- revalidation claim's OR over (policy_version, bundle_sha) could never be
-- served by the policy_version prefix alone anyway — the bundle arm always
-- forced a walk of every judged row, which is also what docs/REPLAY.md
-- documents revalidate to cost. A scan of run_verdicts is that same walk.
-- If a measured need for a cheaper "anything stale?" probe appears, it gets
-- its own index against a real query plan, not a speculative one here.
DROP INDEX runs_stale_policy_idx;

ALTER TABLE runs
    DROP COLUMN server_metrics,
    DROP COLUMN server_score,
    DROP COLUMN validation,
    DROP COLUMN bundle_sha,
    DROP COLUMN policy_version,
    DROP COLUMN validated_at;

-- +goose Down
ALTER TABLE runs
    ADD COLUMN server_metrics jsonb,
    ADD COLUMN server_score   jsonb,
    ADD COLUMN validation     jsonb,
    ADD COLUMN bundle_sha     text,
    ADD COLUMN policy_version smallint,
    ADD COLUMN validated_at   timestamptz;

UPDATE runs r
SET server_metrics = v.server_metrics,
    server_score   = v.server_score,
    validation     = v.validation,
    bundle_sha     = v.bundle_sha,
    policy_version = v.policy_version,
    validated_at   = v.validated_at
FROM run_verdicts v
WHERE v.run_id = r.id;

-- 00017's definition, restored verbatim so the columns can be dropped.
CREATE OR REPLACE VIEW leaderboard_eligible_runs AS
SELECT r.id      AS run_id,
       r.user_id AS user_id,
       r.mode,
       r.duration_ms,
       r.word_count,
       r.lang,
       run_text_source_kind(r.setup)              AS text_source_kind,
       q.id                                       AS quote_id,
       (r.server_score ->> 'total')::bigint       AS score,
       (r.server_metrics ->> 'wpm')::numeric      AS wpm,
       (r.server_metrics ->> 'raw')::numeric      AS raw,
       (r.server_metrics ->> 'accuracy')::numeric AS acc,
       run_mods(r.setup)                          AS mods,
       r.created_at                               AS achieved_at
FROM runs r
         LEFT JOIN LATERAL (SELECT run_quote_id(r.setup) AS quote_id) c ON true
         LEFT JOIN quotes q ON q.id = c.quote_id
WHERE r.status = 'accepted'
  AND run_adopted_from(r.setup) IS NULL
  AND CASE WHEN q.id IS NOT NULL
      THEN true
      ELSE run_text_source_kind(r.setup) = 'seeded'
           AND ((r.mode = 'time'  AND r.duration_ms IN (15000, 30000, 60000))
             OR (r.mode = 'words' AND r.word_count  IN (25, 50, 100)))
      END
  AND jsonb_typeof(r.server_score -> 'total')      = 'number'
  AND jsonb_typeof(r.server_metrics -> 'wpm')      = 'number'
  AND jsonb_typeof(r.server_metrics -> 'raw')      = 'number'
  AND jsonb_typeof(r.server_metrics -> 'accuracy') = 'number';

DROP TABLE run_verdicts;

CREATE INDEX runs_stale_policy_idx ON runs (policy_version, created_at)
    WHERE status <> 'pending';
