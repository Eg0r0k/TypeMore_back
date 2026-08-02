-- +goose Up
-- Player reports (docs/REPORTS.md): one queue for every kind of thing a player
-- can complain about.
--
-- ONE TABLE WITH TYPED FOREIGN KEYS, not `subject_type` + a bare `subject_id`.
-- The polymorphic-key spelling would need no migration to gain a type, and
-- would pay for that with no referential integrity at all: a report could name
-- a quote that never existed, and a deleted subject would leave a row pointing
-- at nothing, with only Go standing between the queue and rubbish. Here the
-- database refuses both. The cost is honest and small — a new subject type is
-- one column, one branch in each CHECK below, and one partial unique index.
--
-- The two CHECKs are deliberately separate concerns:
--
--   * reports_subject_exactly_one makes the row well-formed: exactly one
--     subject column is set, AND it is the one subject_type names. Without the
--     second half a row could claim 'quote' while carrying a user id, and every
--     read would then have to distrust the discriminator it just read.
--   * reports_reason_matches_subject makes the row MEANINGFUL: 'typo' is a
--     thing a quote can have and a player cannot. The vocabulary lives here
--     rather than in Go — unlike the permission map, which is versioned with
--     the binary that enforces it — because a reason is stored forever and the
--     queue is filtered by it. A typo'd reason string would be an unfilterable
--     row in a history nobody can retroactively fix.
CREATE TABLE reports (
    id           uuid PRIMARY KEY   DEFAULT gen_random_uuid(),
    subject_type text      NOT NULL CHECK (subject_type IN ('user', 'quote', 'run')),

    -- ON DELETE CASCADE: a report about something that no longer exists is not
    -- a moderation record, it is a dangling row. Accounts are not deletable
    -- today (no code path calls DeleteUser), so this is a guarantee about the
    -- future rather than a behaviour anyone will see now.
    subject_user_id  uuid REFERENCES users (id) ON DELETE CASCADE,
    subject_quote_id uuid REFERENCES quotes (id) ON DELETE CASCADE,
    subject_run_id   uuid REFERENCES runs (id) ON DELETE CASCADE,

    reporter_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    reason      text NOT NULL,
    comment     text,

    status     text        NOT NULL DEFAULT 'open'
        CHECK (status IN ('open', 'actioned', 'dismissed')),
    created_at timestamptz NOT NULL DEFAULT now(),

    resolved_at     timestamptz,
    -- ON DELETE SET NULL, like a ban's actor columns (00023): the decision
    -- outlives the account that made it. This column is therefore NOT part of
    -- the resolution CHECK below — deleting a moderator must not make a
    -- historical row violate a constraint.
    resolved_by     uuid REFERENCES users (id) ON DELETE SET NULL,
    resolution_note text,

    CONSTRAINT reports_subject_exactly_one CHECK (
        num_nonnulls(subject_user_id, subject_quote_id, subject_run_id) = 1
            AND (subject_type <> 'user' OR subject_user_id IS NOT NULL)
            AND (subject_type <> 'quote' OR subject_quote_id IS NOT NULL)
            AND (subject_type <> 'run' OR subject_run_id IS NOT NULL)),

    CONSTRAINT reports_reason_matches_subject CHECK (
        CASE subject_type
            WHEN 'user' THEN reason IN ('offensive_name', 'impersonation', 'cheating', 'other')
            WHEN 'quote' THEN reason IN ('typo', 'wrong_language', 'offensive', 'other')
            WHEN 'run' THEN reason IN ('cheating', 'impossible_score', 'other')
            END),

    CONSTRAINT reports_comment_length CHECK (
        comment IS NULL OR char_length(comment) BETWEEN 1 AND 1000),

    -- status and resolved_at move together, in both directions: an open report
    -- with a resolution timestamp and a closed one without are equally
    -- impossible to render honestly.
    CONSTRAINT reports_resolution_complete CHECK ((status = 'open') = (resolved_at IS NULL)),

    -- Reporting yourself is not a moderation signal. In SQL rather than in the
    -- handler because it is a property of the ROW, and rows arrive by whatever
    -- path exists next year.
    CONSTRAINT reports_no_self_report CHECK (subject_user_id IS DISTINCT FROM reporter_id)
);

-- One OPEN report per (reporter, subject) — three indexes, one per subject
-- column, which is the price of typed foreign keys and is paid once here.
--
-- Partial on `status = 'open'` on purpose: a resolved report must not block a
-- new one. If the same player misbehaves again after a dismissal, that is a new
-- incident and the queue has to be able to hear about it.
CREATE UNIQUE INDEX reports_open_user_uniq ON reports (reporter_id, subject_user_id)
    WHERE status = 'open' AND subject_user_id IS NOT NULL;
CREATE UNIQUE INDEX reports_open_quote_uniq ON reports (reporter_id, subject_quote_id)
    WHERE status = 'open' AND subject_quote_id IS NOT NULL;
CREATE UNIQUE INDEX reports_open_run_uniq ON reports (reporter_id, subject_run_id)
    WHERE status = 'open' AND subject_run_id IS NOT NULL;

-- The queue read: group the open reports by subject, newest pressure first.
-- Partial on the status the queue actually asks for, so the index holds only
-- the working set and not the whole history of everything ever resolved.
CREATE INDEX reports_queue_idx
    ON reports (subject_type, subject_user_id, subject_quote_id, subject_run_id)
    WHERE status = 'open';

-- Reads of one reporter's history: the rate-limit's companion for spotting
-- somebody whose reports are all dismissed (docs/REPORTS.md, "Abuse").
CREATE INDEX reports_reporter_idx ON reports (reporter_id, created_at DESC);

-- +goose Down
DROP TABLE reports;
