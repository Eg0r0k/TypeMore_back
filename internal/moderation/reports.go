package moderation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Reports (docs/REPORTS.md): the signal half of moderation, next to bans, which
// are the action half.
//
// The design rule this file exists to hold: a report RECORDS a decision, it
// never performs one. Resolving a queue item writes a verdict and nothing else;
// banning the player or withdrawing the quote happens through those surfaces,
// which already own their own invariants and audit trail. That is not
// squeamishness about convenience — it is what keeps this package from having
// to reach into the quote domain, which the layering rules forbid and which
// would give bans a second birthplace.

// ErrUnknownSubject reports a subject type this build does not know. It is a
// client error, not a server one: the vocabulary is closed and versioned.
var ErrUnknownSubject = errors.New("moderation: unknown report subject type")

// ErrUnknownReason reports a reason that is not in the subject type's set.
// Checked here AND by a CHECK constraint — the constraint is the guarantee, this
// is the good error message.
var ErrUnknownReason = errors.New("moderation: reason does not apply to that subject")

// ErrSubjectMissing reports a report filed against something that does not
// exist. It is detected by the foreign keys rather than by a lookup first: a
// check-then-insert would be both a race and a second round trip for an answer
// the INSERT already has.
var ErrSubjectMissing = errors.New("moderation: no such subject")

// SubjectType names what a report is about. The values are the stored
// discriminator; adding one is a migration (a column, two CHECK branches and a
// partial unique index — docs/REPORTS.md, "Adding a subject type").
type SubjectType string

// The three things v1 can be reported.
const (
	SubjectUser  SubjectType = "user"
	SubjectQuote SubjectType = "quote"
	SubjectRun   SubjectType = "run"
)

// reasonsBySubject is the reason vocabulary, mirroring the CHECK constraint in
// migration 00026. The database is the authority — a row that gets past this map
// still cannot be stored — and this copy exists so a client gets "that reason
// does not apply to a quote" instead of a constraint violation rendered as 500.
var reasonsBySubject = map[SubjectType][]string{
	SubjectUser:  {"offensive_name", "impersonation", "cheating", "other"},
	SubjectQuote: {"typo", "wrong_language", "offensive", "other"},
	SubjectRun:   {"cheating", "impossible_score", "other"},
}

// Subject is what a report is about: a type and the id of that type's row.
// Kept as one value so it travels through the service and the store without
// ever becoming three parallel arguments that can disagree.
type Subject struct {
	Type SubjectType
	ID   uuid.UUID
}

// Validate reports whether the subject names a known type and carries an id.
func (s Subject) Validate() error {
	if _, ok := reasonsBySubject[s.Type]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownSubject, s.Type)
	}
	if s.ID == uuid.Nil {
		return fmt.Errorf("%w: no subject id", ErrUnknownSubject)
	}
	return nil
}

// ReasonValid reports whether reason belongs to this subject type's set.
func (s Subject) ReasonValid(reason string) bool {
	for _, r := range reasonsBySubject[s.Type] {
		if r == reason {
			return true
		}
	}
	return false
}

// Columns splits the subject into the three nullable foreign keys the table
// carries, exactly one of which is set. This is the ONLY place that mapping
// exists: adding a subject type changes this function and the migration, and
// nothing else has to know the shape.
func (s Subject) Columns() (user, quote, run *uuid.UUID) {
	id := s.ID
	switch s.Type {
	case SubjectUser:
		return &id, nil, nil
	case SubjectQuote:
		return nil, &id, nil
	case SubjectRun:
		return nil, nil, &id
	}
	return nil, nil, nil
}

// Report is one filed complaint.
type Report struct {
	ID        uuid.UUID
	Subject   Subject
	Reporter  uuid.UUID
	Reason    string
	Comment   string
	Status    string
	CreatedAt time.Time
}

// The three states a report can be in. A report is opened by a player and
// closed by a moderator; there is no in-between state, because there is one
// admin role and a claimed-by field nobody reads is a field that goes stale.
const (
	StatusOpen      = "open"
	StatusActioned  = "actioned"
	StatusDismissed = "dismissed"
)

// QueueItem is one SUBJECT in the moderator's queue, with its open reports
// folded together — the unit a moderator actually decides about.
type QueueItem struct {
	Subject       Subject
	OpenReports   int64
	FirstReported time.Time
	LastReported  time.Time
	// Reasons are the distinct reasons given, sorted. "12 reports, all
	// 'offensive'" and "12 reports, 6 different reasons" are different
	// situations and the queue has to be able to show which.
	Reasons []string
	// Snapshot describes the subject as it stands NOW, resolved in the same
	// query: the display name, the quote's text, the run's owner and status.
	// It is what lets a moderator triage without opening each item, and it
	// carries the subject's current moderation state so an already-handled
	// subject is obvious at a glance.
	Snapshot SubjectSnapshot
}

// SubjectSnapshot is the queue's view of the thing being reported. Exactly the
// fields belonging to the item's own type are populated.
type SubjectSnapshot struct {
	// UserName is set for a user subject.
	UserName string
	// QuoteText / QuoteLang / QuoteWithdrawn are set for a quote subject.
	QuoteText      string
	QuoteLang      string
	QuoteWithdrawn bool
	// RunOwnerName / RunStatus are set for a run subject.
	RunOwnerName string
	RunStatus    string
}

// SubjectReport is one report in the detail view behind a queue item, with the
// names resolved.
type SubjectReport struct {
	ID             uuid.UUID
	Reason         string
	Comment        string
	Status         string
	CreatedAt      time.Time
	ResolvedAt     *time.Time
	ResolutionNote string
	ReporterName   string
	ResolverName   string
}

// FileResult reports what filing did. Created is false when the reporter
// already had an open report on that subject — the answer is the existing
// report and a 200, because a double-tapped button is not a client error.
type FileResult struct {
	Report  Report
	Created bool
}

// ReportStore is the persistence this half of the domain needs.
type ReportStore interface {
	File(ctx context.Context, subject Subject, reporter uuid.UUID, reason, comment string) (FileResult, error)
	Queue(ctx context.Context, subjectType *SubjectType, limit int32) ([]QueueItem, error)
	ForSubject(ctx context.Context, subject Subject, limit int32) ([]SubjectReport, error)
	// Resolve closes every open report on the subject, returning how many moved.
	// Zero means there was nothing open, which the caller answers idempotently.
	Resolve(ctx context.Context, subject Subject, status string, resolver uuid.UUID, note string) (int64, error)
	// OpenBy counts a reporter's outstanding reports — the breadth cap that the
	// rate limiter cannot express.
	OpenBy(ctx context.Context, reporter uuid.UUID) (int64, error)
	// IsRestricted is the ban gate, the same one that stops a banned player's
	// runs counting. A banned account may not file: the queue is for signal
	// from players in good standing.
	IsRestricted(ctx context.Context, userID uuid.UUID) (bool, error)
}
