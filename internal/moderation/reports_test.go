package moderation_test

// Reports (docs/REPORTS.md) at the store level: what the DATABASE guarantees,
// and what the grouping reads actually return.
//
// The constraint tests matter more than they look. The whole extensibility
// argument for one table with typed foreign keys rests on the database
// refusing malformed rows — if a report could name a quote while carrying a
// user id, every read downstream would have to distrust the discriminator it
// just read, and the single-queue design would fall apart.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/moderation"
)

func ctx() context.Context { return context.Background() }

// quoteRow inserts a minimal published quote to report.
func (h *harness) quoteRow(t *testing.T, text string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, h.pool.QueryRow(ctx(),
		`INSERT INTO quotes (id, lang, upstream_id, text, source, length, len_group, text_hash)
		 VALUES (gen_random_uuid(), 'english', floor(random()*1000000)::int, $1, 'test', $2, 0, md5($1))
		 RETURNING id`, text, len(text)).Scan(&id))
	return id
}

// runRow inserts a minimal accepted run to report.
func (h *harness) runRow(t *testing.T, owner uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, h.pool.QueryRow(ctx(),
		`INSERT INTO runs (user_id, mode, duration_ms, lang, seed, dict_hash,
		                   setup, client_metrics, client_score, score_version,
		                   status, log, log_bytes)
		 VALUES ($1, 'time', 15000, 'english', 42, 'deadbeef',
		         '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, 1, 'accepted', '\x00'::bytea, 1)
		 RETURNING id`, owner).Scan(&id))
	return id
}

// The three subject types round-trip: filed, found in the queue under the right
// discriminator, and readable back as the same subject.
func TestReportsAcceptEverySubjectType(t *testing.T) {
	h := newHarness(t)
	reporter := h.user(t, "reporter1")
	offender := h.user(t, "offender1")
	quoteID := h.quoteRow(t, "a quote with a typpo")
	runID := h.runRow(t, offender)

	subjects := []moderation.Subject{
		{Type: moderation.SubjectUser, ID: offender},
		{Type: moderation.SubjectQuote, ID: quoteID},
		{Type: moderation.SubjectRun, ID: runID},
	}
	reasons := []string{"offensive_name", "typo", "cheating"}
	for i, s := range subjects {
		res, err := h.store.File(ctx(), s, reporter, reasons[i], "please look")
		require.NoError(t, err, "subject %s", s.Type)
		assert.True(t, res.Created)
		assert.Equal(t, s, res.Report.Subject)
		assert.Equal(t, moderation.StatusOpen, res.Report.Status)
	}

	queue, err := h.store.Queue(ctx(), nil, 50)
	require.NoError(t, err)
	require.Len(t, queue, 3, "three subjects, three queue items")
	seen := map[moderation.SubjectType]uuid.UUID{}
	for _, item := range queue {
		seen[item.Subject.Type] = item.Subject.ID
		assert.EqualValues(t, 1, item.OpenReports)
	}
	assert.Equal(t, map[moderation.SubjectType]uuid.UUID{
		moderation.SubjectUser:  offender,
		moderation.SubjectQuote: quoteID,
		moderation.SubjectRun:   runID,
	}, seen)
}

// The queue is per SUBJECT: many complaints about one thing are one decision.
// The counts and the reason set are what a moderator triages on.
func TestQueueGroupsBySubjectAndCounts(t *testing.T) {
	h := newHarness(t)
	quoteID := h.quoteRow(t, "something offensive")
	quiet := h.quoteRow(t, "a mildly wrong one")

	for i, name := range []string{"rep_a1", "rep_a2", "rep_a3"} {
		reporter := h.user(t, name)
		reason := "offensive"
		if i == 2 {
			reason = "wrong_language"
		}
		_, err := h.store.File(ctx(), moderation.Subject{Type: moderation.SubjectQuote, ID: quoteID},
			reporter, reason, "")
		require.NoError(t, err)
	}
	_, err := h.store.File(ctx(), moderation.Subject{Type: moderation.SubjectQuote, ID: quiet},
		h.user(t, "rep_b1"), "typo", "")
	require.NoError(t, err)

	queue, err := h.store.Queue(ctx(), nil, 50)
	require.NoError(t, err)
	require.Len(t, queue, 2)

	// Most-reported first: pressure orders the queue, so the loudest thing is
	// what a moderator opens first.
	assert.Equal(t, quoteID, queue[0].Subject.ID)
	assert.EqualValues(t, 3, queue[0].OpenReports)
	assert.Equal(t, []string{"offensive", "wrong_language"}, queue[0].Reasons,
		"distinct reasons, sorted")
	assert.Equal(t, "something offensive", queue[0].Snapshot.QuoteText,
		"the snapshot lets a moderator triage without opening the item")
	assert.False(t, queue[0].Snapshot.QuoteWithdrawn)

	assert.Equal(t, quiet, queue[1].Subject.ID)
	assert.EqualValues(t, 1, queue[1].OpenReports)

	// The type filter narrows the queue without changing the grouping.
	quoteType := moderation.SubjectQuote
	only, err := h.store.Queue(ctx(), &quoteType, 50)
	require.NoError(t, err)
	assert.Len(t, only, 2)
	userType := moderation.SubjectUser
	none, err := h.store.Queue(ctx(), &userType, 50)
	require.NoError(t, err)
	assert.Empty(t, none)
}

// A second report from the same person on the same subject is the SAME report,
// not a duplicate and not an error — one person cannot inflate the count by
// tapping twice.
func TestRepeatReportIsIdempotent(t *testing.T) {
	h := newHarness(t)
	reporter := h.user(t, "double")
	offender := h.user(t, "targeted")
	subject := moderation.Subject{Type: moderation.SubjectUser, ID: offender}

	first, err := h.store.File(ctx(), subject, reporter, "offensive_name", "rude")
	require.NoError(t, err)
	require.True(t, first.Created)

	second, err := h.store.File(ctx(), subject, reporter, "impersonation", "different reason")
	require.NoError(t, err)
	assert.False(t, second.Created, "no second row")
	assert.Equal(t, first.Report.ID, second.Report.ID, "the original report is returned")
	assert.Equal(t, "offensive_name", second.Report.Reason,
		"and it keeps its original reason — the first statement stands")

	queue, err := h.store.Queue(ctx(), nil, 50)
	require.NoError(t, err)
	require.Len(t, queue, 1)
	assert.EqualValues(t, 1, queue[0].OpenReports, "one person, one report")

	// A DIFFERENT reporter is a genuine second signal.
	_, err = h.store.File(ctx(), subject, h.user(t, "somebodyelse"), "offensive_name", "")
	require.NoError(t, err)
	queue, err = h.store.Queue(ctx(), nil, 50)
	require.NoError(t, err)
	assert.EqualValues(t, 2, queue[0].OpenReports)
}

// Resolving decides about the SUBJECT: every open report on it closes at once,
// and the queue item disappears whole rather than shedding one complaint.
func TestResolveClosesTheWholeGroup(t *testing.T) {
	h := newHarness(t)
	moderator := h.user(t, "themod")
	quoteID := h.quoteRow(t, "bad quote")
	subject := moderation.Subject{Type: moderation.SubjectQuote, ID: quoteID}
	for _, name := range []string{"rep_c1", "rep_c2", "rep_c3"} {
		_, err := h.store.File(ctx(), subject, h.user(t, name), "offensive", "")
		require.NoError(t, err)
	}

	n, err := h.store.Resolve(ctx(), subject, moderation.StatusActioned, moderator, "withdrawn the quote")
	require.NoError(t, err)
	assert.EqualValues(t, 3, n, "all three moved together")

	queue, err := h.store.Queue(ctx(), nil, 50)
	require.NoError(t, err)
	assert.Empty(t, queue, "nothing open left")

	// The history survives, with the verdict and its author attached.
	reports, err := h.store.ForSubject(ctx(), subject, 50)
	require.NoError(t, err)
	require.Len(t, reports, 3)
	for _, rep := range reports {
		assert.Equal(t, moderation.StatusActioned, rep.Status)
		assert.NotNil(t, rep.ResolvedAt)
		assert.Equal(t, "themod", rep.ResolverName)
		assert.Equal(t, "withdrawn the quote", rep.ResolutionNote)
	}

	// Resolving again closes nothing and is not an error: somebody else may
	// simply have got there first.
	n, err = h.store.Resolve(ctx(), subject, moderation.StatusDismissed, moderator, "")
	require.NoError(t, err)
	assert.EqualValues(t, 0, n)

	// And a resolved report does not block a NEW one: a repeat offence is a new
	// incident, which is why the unique index is partial on status='open'.
	res, err := h.store.File(ctx(), subject, h.user(t, "rep_c1again"), "offensive", "")
	require.NoError(t, err)
	assert.True(t, res.Created)
}

// The reporter cap is what the rate limiter cannot express: how much of the
// queue one person may occupy at once.
func TestOpenByCountsOnlyOpenReports(t *testing.T) {
	h := newHarness(t)
	reporter := h.user(t, "prolific")
	moderator := h.user(t, "mod2")
	first := h.quoteRow(t, "one")
	second := h.quoteRow(t, "two")
	for _, id := range []uuid.UUID{first, second} {
		_, err := h.store.File(ctx(), moderation.Subject{Type: moderation.SubjectQuote, ID: id},
			reporter, "typo", "")
		require.NoError(t, err)
	}
	n, err := h.store.OpenBy(ctx(), reporter)
	require.NoError(t, err)
	assert.EqualValues(t, 2, n)

	_, err = h.store.Resolve(ctx(), moderation.Subject{Type: moderation.SubjectQuote, ID: first},
		moderation.StatusDismissed, moderator, "")
	require.NoError(t, err)
	n, err = h.store.OpenBy(ctx(), reporter)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n, "resolving frees the reporter's budget")
}

// Reporting something that does not exist is caught by the foreign keys and
// surfaces as a domain error, not as a 500 carrying a pg code.
func TestReportOnMissingSubjectIsRejected(t *testing.T) {
	h := newHarness(t)
	_, err := h.store.File(ctx(),
		moderation.Subject{Type: moderation.SubjectQuote, ID: uuid.New()},
		h.user(t, "hopeful"), "typo", "")
	assert.ErrorIs(t, err, moderation.ErrSubjectMissing)
}

// --- what the DATABASE refuses -----------------------------------------------

// The row must be well-formed AND meaningful: the discriminator must agree with
// which column is set, and the reason must belong to the subject type. Both are
// CHECK constraints, so no code path can produce a row that violates them.
func TestSchemaRefusesMalformedReports(t *testing.T) {
	h := newHarness(t)
	reporter := h.user(t, "prober")
	offender := h.user(t, "victim")
	quoteID := h.quoteRow(t, "some text")

	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{
			name: "discriminator disagreeing with the column",
			sql: `INSERT INTO reports (subject_type, subject_quote_id, reporter_id, reason)
			      VALUES ('user', $1, $2, 'offensive_name')`,
			args: []any{quoteID, reporter},
		},
		{
			name: "two subjects at once",
			sql: `INSERT INTO reports (subject_type, subject_user_id, subject_quote_id, reporter_id, reason)
			      VALUES ('user', $1, $2, $3, 'offensive_name')`,
			args: []any{offender, quoteID, reporter},
		},
		{
			name: "no subject at all",
			sql: `INSERT INTO reports (subject_type, reporter_id, reason)
			      VALUES ('user', $1, 'offensive_name')`,
			args: []any{reporter},
		},
		{
			name: "a reason that belongs to another subject type",
			sql: `INSERT INTO reports (subject_type, subject_user_id, reporter_id, reason)
			      VALUES ('user', $1, $2, 'typo')`,
			args: []any{offender, reporter},
		},
		{
			name: "reporting yourself",
			sql: `INSERT INTO reports (subject_type, subject_user_id, reporter_id, reason)
			      VALUES ('user', $1, $1, 'offensive_name')`,
			args: []any{reporter},
		},
		{
			name: "closed with no resolution timestamp",
			sql: `INSERT INTO reports (subject_type, subject_user_id, reporter_id, reason, status)
			      VALUES ('user', $1, $2, 'offensive_name', 'actioned')`,
			args: []any{offender, reporter},
		},
		{
			name: "an unknown status",
			sql: `INSERT INTO reports (subject_type, subject_user_id, reporter_id, reason, status)
			      VALUES ('user', $1, $2, 'offensive_name', 'maybe')`,
			args: []any{offender, reporter},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.pool.Exec(ctx(), tc.sql, tc.args...)
			assert.Error(t, err, "the database must refuse this row")
		})
	}
}

// Subject.Columns and its inverse are the only place the type→column mapping
// lives; a round trip through the store is what proves they agree.
func TestSubjectValidation(t *testing.T) {
	assert.NoError(t, moderation.Subject{Type: moderation.SubjectUser, ID: uuid.New()}.Validate())
	assert.ErrorIs(t, moderation.Subject{Type: "room", ID: uuid.New()}.Validate(),
		moderation.ErrUnknownSubject)
	assert.ErrorIs(t, moderation.Subject{Type: moderation.SubjectUser}.Validate(),
		moderation.ErrUnknownSubject)

	quote := moderation.Subject{Type: moderation.SubjectQuote, ID: uuid.New()}
	assert.True(t, quote.ReasonValid("typo"))
	assert.False(t, quote.ReasonValid("offensive_name"), "that reason is a user's, not a quote's")
	assert.True(t, moderation.Subject{Type: moderation.SubjectRun}.ReasonValid("impossible_score"))
}
