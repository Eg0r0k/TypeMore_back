-- +goose Up
-- Relay v0 persistence (docs/MATCH.md, docs/PROTOCOL.md §6). A match is the
-- record of one multiplayer race: its frozen configuration and, per participant,
-- the authoritative captured event log (the server-stamped batch stream). This
-- phase LANDS the capture only — no validation. The future replay worker reads
-- match_runs.log to recompute metrics/score, exactly like the solo `runs` table.

CREATE TABLE matches (
    -- The wire matchId (see Countdown/event_batch), used verbatim as the PK so
    -- the log stream and the DB row share one identifier.
    id          text        PRIMARY KEY,
    room_code   text        NOT NULL,
    name        text        NOT NULL,

    -- Frozen room settings (incl. textSource) and the per-player freemod snapshot
    -- ([{playerId, freemods}]) exactly as they went out in the countdown. Opaque
    -- to Go, stored as jsonb untouched.
    settings    jsonb       NOT NULL,
    freemods    jsonb       NOT NULL,

    -- mulberry32 is a 32-bit PRNG, so the seed is an unsigned 32-bit integer
    -- (PROTOCOL.md §4). Stored as bigint since Postgres int is signed.
    seed        bigint      NOT NULL CHECK (seed BETWEEN 0 AND 4294967295),
    dict_hash   text        NOT NULL,
    lang        text        NOT NULL,

    go_at       timestamptz NOT NULL,
    ended_at    timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE match_runs (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    match_id     text        NOT NULL REFERENCES matches (id) ON DELETE CASCADE,

    -- The peer-visible player id and display nick at match time. user_id links to
    -- the account for an authenticated player and is NULL for a guest; SET NULL on
    -- account deletion keeps the immutable capture intact.
    player_id    text        NOT NULL,
    nick         text        NOT NULL,
    user_id      uuid        REFERENCES users (id) ON DELETE SET NULL,

    -- This player's frozen freemods and the authoritative capture: the ordered
    -- CapturedBatch stream (batchSeq + recvServerMs + opaque events), gzip'd. This
    -- is the input the replay worker will validate.
    freemods     jsonb       NOT NULL,
    log          bytea       NOT NULL,
    batch_count  int         NOT NULL CHECK (batch_count >= 0),

    final_status text        NOT NULL CHECK (final_status IN ('finished', 'dnf', 'left')),
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Runs of one match (persist/read together) and the future per-user history feed.
CREATE INDEX match_runs_match_idx ON match_runs (match_id);
CREATE INDEX match_runs_user_idx ON match_runs (user_id) WHERE user_id IS NOT NULL;

-- +goose Down
DROP TABLE match_runs;
DROP TABLE matches;
