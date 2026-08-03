-- +goose Up
--
-- One seat per ACCOUNT per match — the schema half of the rule the relay now
-- enforces in memory (docs/PROTOCOL.md §5, internal/ws/registry.go).
--
-- 00021 promoted "one seat per player" into the schema and stopped a persist bug
-- from writing the same seat twice. It could not stop the thing that was
-- actually happening: a seat was minted per CONNECTION, so one account with two
-- tabs open was two players, with two player_ids, and (match_id, player_id)
-- called that perfectly legal. A match against yourself persisted silently and
-- every consumer downstream — a history feed, a per-user aggregate, the replay
-- worker judging the capture — would read it as a real race between two people.
--
-- The relay is where this is fixed; this index is the backstop. It is the same
-- bargain 00021 struck: no ON CONFLICT anywhere in internal/ws/wspg, because a
-- duplicate here is not a retry to absorb, it is the seat rule having failed,
-- and a loud write error is the only way anyone finds out.
--
-- PARTIAL because guests have no account. user_id IS NULL for every guest seat,
-- and a plain UNIQUE would let exactly one guest per match through — NULLs do
-- not collide under UNIQUE by default, but spelling the predicate out means the
-- index is also small (authenticated seats only) and its intent is not left to
-- the reader's memory of three-valued logic.

-- +goose StatementBegin
DO $$
DECLARE
    offenders bigint;
BEGIN
    SELECT count(*) INTO offenders
    FROM (
        SELECT match_id
        FROM match_runs
        WHERE user_id IS NOT NULL
        GROUP BY match_id, user_id
        HAVING count(*) > 1
    ) AS dupes;

    IF offenders > 0 THEN
        -- Deliberately explicit rather than letting CREATE UNIQUE INDEX fail
        -- with "could not create unique index ... duplicate key value". Published
        -- captures are frozen forever, so the operator cannot simply delete the
        -- offending rows; they have to decide which capture is the real one, and
        -- the message has to say that instead of naming a page in an index.
        RAISE EXCEPTION
            'match_runs already holds % (match_id, user_id) group(s) with more than one row', offenders
            USING HINT = 'These are matches an account played against itself before the one-seat-per-account rule existed. '
                         'Inspect them (SELECT match_id, user_id, count(*) FROM match_runs WHERE user_id IS NOT NULL '
                         'GROUP BY 1, 2 HAVING count(*) > 1) and resolve each before applying this migration; '
                         'the captures are immutable, so which row survives is a judgement call, not a cleanup.';
    END IF;
END $$;
-- +goose StatementEnd

CREATE UNIQUE INDEX match_runs_one_seat_per_user
    ON match_runs (match_id, user_id)
    WHERE user_id IS NOT NULL;

-- match_runs_user_idx (00003) stays. This index leads with match_id, so it
-- cannot answer "every match this account played" — the two cover opposite
-- access paths and neither retires the other.

-- +goose Down
DROP INDEX match_runs_one_seat_per_user;
