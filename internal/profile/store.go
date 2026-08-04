package profile

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound reports a display name that resolves to no account. It is the
// public surface's only "who" error: an unknown name is 404 and nothing else
// (names are already public on every board, so there is no enumeration story
// to blur it for).
var ErrNotFound = errors.New("profile: not found")

// MetricStats is one profile metric's three aggregates. Acc and consistency
// travel as [0, 1] fractions, exactly as the core reports them; formatting as
// a percentage is the display edge's job.
type MetricStats struct {
	Highest       float64 `json:"highest"`
	Average       float64 `json:"average"`
	AverageLast10 float64 `json:"averageLast10"`
}

// Streak is the consecutive-days-played pair. Days are runs-based and bucketed
// in UTC — logins are not tracked, by design.
type Streak struct {
	Current int32 `json:"current"`
	Best    int32 `json:"best"`
}

// Summary is GET /profile/summary: identity, counters, the four metric groups,
// the streaks, and per-language play counts.
type Summary struct {
	DisplayName string    `json:"displayName"`
	Joined      time.Time `json:"joined"`
	// TestsStarted = TestsCompleted + the client-reported restart counts
	// (docs/RUNS.md, restartsSinceLastSubmit): a restarted test never became a
	// row, so the completed count alone would undercount every starter.
	TestsStarted   int64 `json:"testsStarted"`
	TestsCompleted int64 `json:"testsCompleted"`
	// RestartsPerCompleted is the ratio the counters row renders; 0 for a
	// fresh account rather than a division error.
	RestartsPerCompleted float64     `json:"restartsPerCompleted"`
	TimeTypingMs         int64       `json:"timeTypingMs"`
	EstimatedWordsTyped  int64       `json:"estimatedWordsTyped"`
	Wpm                  MetricStats `json:"wpm"`
	Raw                  MetricStats `json:"raw"`
	Acc                  MetricStats `json:"acc"`
	Consistency          MetricStats `json:"consistency"`
	Streak               Streak      `json:"streak"`
	// Languages is [{"lang": "...", "tests": N}], most played first — built in
	// SQL and passed through verbatim.
	Languages json.RawMessage `json:"languages"`
}

// ActivityDay is one populated day of the activity calendar. Days without runs
// are absent — the calendar renders its own gaps.
type ActivityDay struct {
	Date   time.Time
	Tests  int32
	TimeMs int64
}

// HistogramBucket is one 10-wpm histogram bar; Bucket is the lower bound.
type HistogramBucket struct {
	Bucket int32 `json:"wpm"`
	Tests  int32 `json:"tests"`
}

// TimeseriesDay is one day of the charts series.
type TimeseriesDay struct {
	Date         time.Time
	TimeTypingMs int64
	AvgWpm       float64
	AvgAcc       float64
}

// PB is one personal-best card: a leaderboard_entries row, verbatim — the
// entries table already stores exactly one best run per (player, bucket).
type PB struct {
	BucketKey   string
	RunID       uuid.UUID
	Score       int64
	Wpm         float64
	Raw         float64
	Acc         float64
	Grade       string
	Mods        json.RawMessage
	QuoteSource *string
	AchievedAt  time.Time
}

// KeyboardKey is one physical key's lifetime aggregates from the
// user_keyboard_profile projection (docs/PROFILE.md, "Keyboard").
type KeyboardKey struct {
	KeyID         string
	Presses       int64
	Errors        int64
	IntervalSumMs float64
	IntervalCount int64
}

// PublicUser is the public surface's name resolution: the row the header
// answers from, and the two switches every other public route gates on.
type PublicUser struct {
	ID             uuid.UUID
	DisplayName    string
	Joined         time.Time
	ProfilePublic  bool
	KeyboardPublic bool
}

// SearchResult is one hit of the player search: the same three fields the
// header answers with, and nothing else. Search is a way to FIND a profile,
// never a second way to read one — every aggregate stays behind the per-profile
// gates, so a closed profile appearing in these results discloses exactly what
// a leaderboard already does, that the name exists. ProfilePublic rides along
// so a client can render the closed state rather than linking to a page it is
// about to be 403'd from.
type SearchResult struct {
	DisplayName   string
	Joined        time.Time
	ProfilePublic bool
}

// PublicRun is one row of a profile's public run history — an explicit
// allowlist, narrower than the owner's own runs feed: the server's verdict
// numbers and the derived display cells, and nothing the run's owner reported
// or the moderation trail recorded. Every run here is accepted by
// construction (the query's predicate), so no Status field exists to leak
// anything else.
type PublicRun struct {
	ID            uuid.UUID
	Mode          string
	DurationMs    *int32
	WordCount     *int32
	Lang          string
	ServerMetrics json.RawMessage
	ServerScore   json.RawMessage
	CreatedAt     time.Time
	// The derived cells, unpacked exactly as the runs domain unpacks its
	// summaries (docs/RUNS.md): grade/consistency/chars always present on
	// these rows (all judged), quoteId on quote runs, adoptedFromRunId on
	// seeded repeats, mods always.
	Grade            *string
	Consistency      *float64
	Chars            json.RawMessage
	QuoteID          *uuid.UUID
	AdoptedFromRunID *uuid.UUID
	Mods             json.RawMessage
}

// RunCursor is the public history's keyset position, ordered like the owner's
// feed: (created_at, id) descending.
type RunCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// Identity is the profile's self-described half (00029): what the owner wrote
// about themselves. Both fields are nil when never set — NULL and "" are
// deliberately different states in the schema, so a client never has to guess
// whether an empty bio was cleared or never written.
type Identity struct {
	Bio      *string
	Keyboard *string
}

// ShowcaseBadge is one badge as the PUBLIC surface sees it: a code and where
// its owner put it. Nothing about how it renders — the name, the description,
// the icon and the colour live in the frontend registry, and this server has
// no opinion about any of them (internal/badges).
type ShowcaseBadge struct {
	Code  string
	Order int32
}

// GrantedBadge is one badge the account HOLDS, shown or not: the pool the
// settings screen arranges, and what a showcase write is validated against.
type GrantedBadge struct {
	Code      string
	GrantedAt time.Time
	// Order is nil when the badge is held but hidden.
	Order *int32
}

// Link is one social link: the service and the HANDLE, never a URL. The
// renderer pastes the handle onto a prefix it owns, so the set of hosts this
// product can link to is a list in the source (see links.go).
type Link struct {
	Kind   string
	Handle string
}

// ProfilePatch is one write of the owner-editable profile. Every field is a
// pointer because the route is a PATCH: nil means "not mentioned, leave it",
// which is a different instruction from "set this to empty".
type ProfilePatch struct {
	// Bio/Keyboard, each with an explicit "was this mentioned" flag.
	//
	// A bare *string cannot carry this: the handler normalises a cleared field
	// to NULL, so "clear my bio" and "my request was not about the bio" would
	// both arrive as nil — and the write would silently erase a field nobody
	// touched. SetBio distinguishes them; Bio then says clear (nil) or set.
	Bio         *string
	SetBio      bool
	Keyboard    *string
	SetKeyboard bool
	// Links, keyed by kind. An empty handle removes that link's row — an empty
	// handle is not a state user_links can hold.
	Links map[string]string
	// Showcase is the badge codes to display, in the order given. Non-nil and
	// empty means "show none"; nil means the request was not about badges.
	Showcase []string
}

// Store is the profile read model, implemented by pgstore. Every method is
// scoped to one user; today is the caller's UTC date for the streak boundary.
type Store interface {
	Summary(ctx context.Context, userID uuid.UUID, today time.Time) (Summary, error)
	Activity(ctx context.Context, userID uuid.UUID, since time.Time) ([]ActivityDay, error)
	Histogram(ctx context.Context, userID uuid.UUID) ([]HistogramBucket, error)
	// Timeseries returns the per-day series inside [from, to) plus the OLS
	// slope of wpm over cumulative hours typed in that range (docs/PROFILE.md).
	Timeseries(ctx context.Context, userID uuid.UUID, from, to time.Time) ([]TimeseriesDay, float64, error)
	PBs(ctx context.Context, userID uuid.UUID) ([]PB, error)
	// Keyboard returns the projection's rows plus the user's dominant
	// dictionary language ("" for a fresh account) — the heatmap's default
	// layout is derived from it.
	Keyboard(ctx context.Context, userID uuid.UUID) ([]KeyboardKey, string, error)

	// PublicUser resolves a display name (case-insensitively — the column is
	// citext) to the public surface's identity row, or ErrNotFound.
	PublicUser(ctx context.Context, displayName string) (PublicUser, error)
	// SearchUsers finds accounts whose display name CONTAINS name, case
	// -insensitively, best match first (exact, then prefix, then shortest, then
	// alphabetical — the query owns that order). Banned accounts are never
	// returned; closed ones are. The caller has already bounded name's length;
	// escaping it against LIKE's wildcards is the store's job, because it is a
	// property of how the store asks, not of what the caller meant.
	SearchUsers(ctx context.Context, name string, limit int32) ([]SearchResult, error)
	// PublicRuns lists a profile's publicly visible runs newest-first —
	// accepted only, none while the owner is banned (the query owns that
	// predicate). after=nil means the first page.
	PublicRuns(ctx context.Context, userID uuid.UUID, after *RunCursor, limit int32) ([]PublicRun, error)
	// PublicPBs is PBs through the boards' ban predicate: a banned owner's
	// records are hidden here exactly as they are on every board.
	PublicPBs(ctx context.Context, userID uuid.UUID) ([]PB, error)

	// --- the identity half (00029) ---

	// Identity returns the account's bio/keyboard, both nil when unset.
	Identity(ctx context.Context, userID uuid.UUID) (Identity, error)
	// ShowcaseBadges returns the badges the owner chose to display, in their
	// order. Revoked grants are never returned — the live predicate is the
	// revocation, never display_order (the query explains why).
	ShowcaseBadges(ctx context.Context, userID uuid.UUID) ([]ShowcaseBadge, error)
	// GrantedBadges returns every live grant, shown or not.
	GrantedBadges(ctx context.Context, userID uuid.UUID) ([]GrantedBadge, error)
	// Links returns the account's social links, ordered by kind.
	Links(ctx context.Context, userID uuid.UUID) ([]Link, error)
	// ApplyProfilePatch writes one owner edit ATOMICALLY: the bio, the links
	// and the showcase move together or not at all. A half-applied patch would
	// leave a profile the owner never asked for and cannot see they have.
	ApplyProfilePatch(ctx context.Context, userID uuid.UUID, patch ProfilePatch) error
}
