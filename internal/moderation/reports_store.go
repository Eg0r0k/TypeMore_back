package moderation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/typemore/typemore-server/internal/moderation/moderationdb"
)

// The report half of the store, on the same *Store as bans: one pool, one
// adapter, two subjects. Compile-time proof that it satisfies the interface the
// service depends on.
var _ ReportStore = (*Store)(nil)

// File records one report, or reports that the same person already has an open
// one on that subject.
//
// The uniqueness is enforced by three partial unique indexes (00026), so this
// is a race-free INSERT ... ON CONFLICT DO NOTHING followed by a read of the
// row that won, rather than a check-then-insert that two concurrent taps would
// both pass.
func (s *Store) File(ctx context.Context, subject Subject, reporter uuid.UUID, reason, comment string) (FileResult, error) {
	user, quote, run := subject.Columns()
	row, err := s.q.CreateReport(ctx, moderationdb.CreateReportParams{
		SubjectType:    string(subject.Type),
		SubjectUserID:  user,
		SubjectQuoteID: quote,
		SubjectRunID:   run,
		ReporterID:     reporter,
		Reason:         reason,
		Comment:        nullableText(comment),
	})
	switch {
	case err == nil:
		return FileResult{Report: reportOf(subject, row.ID, row.ReporterID, row.Reason,
			row.Comment, row.Status, row.CreatedAt), Created: true}, nil
	case isForeignKeyViolation(err):
		// One of the three subject foreign keys did not resolve: the thing
		// being reported does not exist. The database is what knows this, and
		// mapping it HERE keeps pg error codes out of the HTTP layer.
		return FileResult{}, ErrSubjectMissing
	case !errors.Is(err, pgx.ErrNoRows):
		return FileResult{}, fmt.Errorf("moderation: create report: %w", err)
	}

	// DO NOTHING returned no row: this reporter already has an open report on
	// this subject. Answer with it — the request expressed a state that already
	// holds, which is the same idempotency the unban route answers with.
	existing, err := s.q.FindOpenReport(ctx, moderationdb.FindOpenReportParams{
		ReporterID:     reporter,
		SubjectType:    string(subject.Type),
		SubjectUserID:  user,
		SubjectQuoteID: quote,
		SubjectRunID:   run,
	})
	if err != nil {
		// A conflict with no findable open report means the row was resolved
		// between the two statements — vanishingly rare, and a retry is the
		// honest answer rather than inventing a result.
		return FileResult{}, fmt.Errorf("moderation: find open report: %w", err)
	}
	return FileResult{Report: reportOf(subject, existing.ID, existing.ReporterID, existing.Reason,
		existing.Comment, existing.Status, existing.CreatedAt), Created: false}, nil
}

// Queue lists open reports grouped by subject, most-reported first.
func (s *Store) Queue(ctx context.Context, subjectType *SubjectType, limit int32) ([]QueueItem, error) {
	var filter *string
	if subjectType != nil {
		t := string(*subjectType)
		filter = &t
	}
	rows, err := s.q.ListReportQueue(ctx, moderationdb.ListReportQueueParams{
		SubjectType: filter,
		RowLimit:    limit,
	})
	if err != nil {
		return nil, fmt.Errorf("moderation: report queue: %w", err)
	}
	out := make([]QueueItem, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		subject, ok := subjectOf(r.SubjectType, r.SubjectUserID, r.SubjectQuoteID, r.SubjectRunID)
		if !ok {
			// A stored subject type this build does not know: skip rather than
			// guess. It can only happen while a rollback is in flight, and a
			// queue missing one row beats a queue that mislabels it.
			continue
		}
		out = append(out, QueueItem{
			Subject:       subject,
			OpenReports:   r.OpenReports,
			FirstReported: r.FirstReported,
			LastReported:  r.LastReported,
			Reasons:       r.Reasons,
			Snapshot: SubjectSnapshot{
				UserName:       deref(r.UserName),
				QuoteText:      deref(r.QuoteText),
				QuoteLang:      deref(r.QuoteLang),
				QuoteWithdrawn: r.QuoteWithdrawn,
				RunOwnerName:   deref(r.RunOwnerName),
				RunStatus:      deref(r.RunStatus),
			},
		})
	}
	return out, nil
}

// ForSubject returns every report on one subject, newest first.
func (s *Store) ForSubject(ctx context.Context, subject Subject, limit int32) ([]SubjectReport, error) {
	user, quote, run := subject.Columns()
	rows, err := s.q.ListReportsForSubject(ctx, moderationdb.ListReportsForSubjectParams{
		SubjectType:    string(subject.Type),
		SubjectUserID:  user,
		SubjectQuoteID: quote,
		SubjectRunID:   run,
		RowLimit:       limit,
	})
	if err != nil {
		return nil, fmt.Errorf("moderation: reports for subject: %w", err)
	}
	out := make([]SubjectReport, len(rows))
	for i := range rows {
		r := &rows[i]
		out[i] = SubjectReport{
			ID: r.ID, Reason: r.Reason, Comment: deref(r.Comment), Status: r.Status,
			CreatedAt: r.CreatedAt, ResolvedAt: r.ResolvedAt,
			ResolutionNote: deref(r.ResolutionNote),
			ReporterName:   r.ReporterName, ResolverName: deref(r.ResolverName),
		}
	}
	return out, nil
}

// Resolve closes every open report on the subject in one statement, so the
// group cannot be left half-decided.
func (s *Store) Resolve(ctx context.Context, subject Subject, status string, resolver uuid.UUID, note string) (int64, error) {
	user, quote, run := subject.Columns()
	n, err := s.q.ResolveSubjectReports(ctx, moderationdb.ResolveSubjectReportsParams{
		Status:         status,
		Resolver:       resolver,
		Note:           nullableText(note),
		SubjectType:    string(subject.Type),
		SubjectUserID:  user,
		SubjectQuoteID: quote,
		SubjectRunID:   run,
	})
	if err != nil {
		return 0, fmt.Errorf("moderation: resolve reports: %w", err)
	}
	return n, nil
}

// OpenBy counts one reporter's outstanding reports.
func (s *Store) OpenBy(ctx context.Context, reporter uuid.UUID) (int64, error) {
	n, err := s.q.CountOpenReportsBy(ctx, reporter)
	if err != nil {
		return 0, fmt.Errorf("moderation: count open reports: %w", err)
	}
	return n, nil
}

// subjectOf rebuilds a Subject from the discriminator and the three columns —
// Subject.Columns' inverse, and the only other place that mapping lives.
func subjectOf(subjectType string, user, quote, run *uuid.UUID) (Subject, bool) {
	switch SubjectType(subjectType) {
	case SubjectUser:
		if user != nil {
			return Subject{Type: SubjectUser, ID: *user}, true
		}
	case SubjectQuote:
		if quote != nil {
			return Subject{Type: SubjectQuote, ID: *quote}, true
		}
	case SubjectRun:
		if run != nil {
			return Subject{Type: SubjectRun, ID: *run}, true
		}
	}
	return Subject{}, false
}

func reportOf(subject Subject, id, reporter uuid.UUID, reason string, comment *string,
	status string, createdAt time.Time) Report {
	return Report{
		ID: id, Subject: subject, Reporter: reporter, Reason: reason,
		Comment: deref(comment), Status: status, CreatedAt: createdAt,
	}
}

// nullableText maps the empty string onto SQL NULL: an absent comment is
// absent, not a zero-length string somebody has to remember to treat as absent.
func nullableText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// isForeignKeyViolation recognises SQLSTATE 23503. Checked by code rather than
// by message so it survives a locale or a version change.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
