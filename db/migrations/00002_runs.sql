-- +goose Up
-- Run ingestion (BACKEND.md §3–4). A run is the immutable record of one finished
-- typing test: its event log (gzip blob), the bucket columns needed for future
-- leaderboards, and the client-reported preview numbers. This phase LANDS runs
-- with structural validation only; the replay worker (goja) later recomputes
-- metrics/score from the log, sets the authoritative status, and fills the
-- score/rating tables. Everything here is deliberately opaque to game logic.

CREATE TABLE runs (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- Bucket columns (SCORING_CONCEPT §4): lifted out of the setup snapshot so
    -- the future leaderboard/queue scans are plain indexed column reads.
    mode         text        NOT NULL,
    -- Exactly one of duration_ms / word_count is set, per mode semantics: time
    -- modes carry a duration, word modes carry a target count. The XOR CHECK is
    -- the schema-level guarantee; the service validates the same up front.
    duration_ms  int         CHECK (duration_ms IS NULL OR duration_ms > 0),
    word_count   int         CHECK (word_count IS NULL OR word_count > 0),
    CONSTRAINT runs_one_dimension CHECK ((duration_ms IS NULL) <> (word_count IS NULL)),

    lang         text        NOT NULL,
    -- mulberry32 is a 32-bit PRNG, so the seed is an unsigned 32-bit integer
    -- (PROTOCOL.md §4 countdown). Stored as bigint since Postgres int is signed.
    seed         bigint      NOT NULL CHECK (seed BETWEEN 0 AND 4294967295),
    dict_hash    text        NOT NULL,

    -- Full CoreConfig+GenerationConfig snapshot the client played with. The goja
    -- replay will need it verbatim; opaque to Go, so stored as jsonb untouched.
    setup          jsonb     NOT NULL,
    -- Client-reported preview numbers (display-only until replay). wpm/raw/acc.
    client_metrics jsonb     NOT NULL,
    -- Client-reported ScoreResult (display-only until replay).
    client_score   jsonb     NOT NULL,
    score_version  smallint  NOT NULL,

    -- Lifecycle: everything lands 'pending'; the replay worker transitions it to
    -- accepted/flagged/rejected. This phase only ever writes 'pending'.
    status       text        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'accepted', 'flagged', 'rejected')),

    -- The raw EventLog JSON, gzip-compressed (immutable blob). log_bytes is the
    -- uncompressed size, kept so callers/metrics can reason about size without
    -- decompressing.
    log          bytea       NOT NULL,
    log_bytes    int         NOT NULL CHECK (log_bytes >= 0),

    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Own-runs listing (keyset pagination on created_at, id): the profile/history
-- feed reads runs newest-first for one user.
CREATE INDEX runs_user_created_idx ON runs (user_id, created_at DESC);
-- The future replay worker's queue scan: only pending rows, oldest first.
CREATE INDEX runs_pending_idx ON runs (status, created_at) WHERE status = 'pending';

-- +goose Down
DROP TABLE runs;
