package leaderboard

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/typemore/typemore-server/internal/runlimits"
)

// Ranked modes. Zen and custom text are unranked by design (SCORING_CONCEPT §2)
// and never reach a bucket at all.
const (
	ModeTime  = "time"
	ModeWords = "words"
)

// Text origins. TextSourceSeeded — text generated from a server-issued seed —
// is the only one that can appear in a language board's key: a quote run is
// ranked inside its quote and nowhere else (SCORING_CONCEPT §6), so
// TextSourceQuote exists here to be REJECTED as a language-board component, not
// to be formatted into one.
const (
	TextSourceSeeded = "seeded"
	TextSourceQuote  = "quote"
)

// quoteKeyPrefix opens the second key space. See Key.
const quoteKeyPrefix = "quote:"

// Dimension bounds. Read from internal/runlimits, which is the same place the
// ingest validator reads them: a bucket can only ever describe a run that was
// allowed in, and "mirroring the ingest validator's" used to be a comment
// between two literals rather than a fact anything enforced.
//
// Note what these bounds are NOT: they are not the RANKED set. A dimension can
// sit inside them and still name no board — 3 000 words is legal to play and
// ranked nowhere. Which sizes rank is leaderboard_eligible_runs' answer, and
// asking it is rankedShape's job, not this bound's.
const (
	maxDurationMs = runlimits.MaxDurationMs
	maxWordCount  = runlimits.MaxWordCount
	maxLangLen    = 32
	maxSourceLen  = 16
)

// The RANKED SET: the sizes a seeded run competes at (SCORING_CONCEPT §4).
//
// Sitting inside the ingest bounds is NOT the same question. A 3 000-word run is
// legal to play, is judged, is scored and lives in its player's history — and
// ranks nowhere, because a board at that size does not exist. 10 minutes is
// absent for the stated reason that sitting out ten minutes is endurance and
// the sample is too small to rank.
//
// These two slices are the Go half of a fact whose other half is SQL
// (leaderboard_eligible_runs' `duration_ms IN (…)` / `word_count IN (…)`). Two
// copies of a set is exactly the drift this codebase keeps writing migrations
// about, so they are not trusted to agree — TestTheRankedSetAgreesWithTheView
// plants a run at every size in and around both and requires the predicate below
// and the view to return the same answer for each. The set is written twice and
// checked once, rather than written twice and hoped over.
var (
	RankedDurationsMs = []int32{15_000, 30_000, 60_000}
	RankedWordCounts  = []int32{25, 50, 100}
)

// IsRankedShape reports whether a run of this mode and size competes on a board.
//
// This is the PROPERTY the projection asks, in place of the enumeration it used
// to imply. The distinction is the whole of this rule: `NewBucket` bounded the
// dimension by what ingestion allows, which let it mint `words:3000:en:seeded` —
// a perfectly well-formed key for a board that can never hold a row. Nothing
// broke, because the SQL underneath then matched nothing and deleted; but "Go
// names a cell that cannot exist and SQL quietly cleans up after it" is a
// deferred failure of the exact class that produced lobbyEntryOf without
// ModeQuote and a quote run with no bucket. The key is now not built at all.
func IsRankedShape(mode string, dimension int32) bool {
	switch mode {
	case ModeTime:
		return contains(RankedDurationsMs, dimension)
	case ModeWords:
		return contains(RankedWordCounts, dimension)
	default:
		return false
	}
}

func contains(set []int32, v int32) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

// ErrInvalidBucket is returned for a bucket that cannot name a board — an
// unknown mode, a dimension outside the ingest bounds, or a component carrying
// characters the key format cannot round-trip.
var ErrInvalidBucket = errors.New("leaderboard: invalid bucket")

// Bucket names one leaderboard. It is one of two shapes:
//
//   - a LANGUAGE board — a mode at one size, in one language, over seeded text;
//   - a QUOTE board — one quote, identified by QuoteID, with no other dimension
//     at all.
//
// Mods are deliberately NOT part of either — they multiply the score
// (SCORING_CONCEPT §2) instead of splitting the board, so a punctuation run and
// a plain one compete directly.
type Bucket struct {
	// Mode is ModeTime or ModeWords. Empty on a quote board.
	Mode string
	// Dimension is the mode's size: milliseconds for time modes, words for word
	// modes. The two never collide in a key because Mode precedes it. Zero on a
	// quote board.
	Dimension int32
	// Lang is the dictionary language the text was generated from. Empty on a
	// quote board: the quote carries its own language and does not share a
	// ranking with any other text in it.
	Lang string
	// TextSource is the textSource.kind the run declared. Always
	// TextSourceSeeded on a language board, empty on a quote board.
	TextSource string
	// QuoteID is the quote this board ranks, and is non-Nil for EXACTLY the
	// quote boards. It is the discriminator: every other field is empty when it
	// is set, and it is Nil when any of them is.
	QuoteID uuid.UUID
}

// IsQuote reports whether this is a per-quote board.
func (b Bucket) IsQuote() bool { return b.QuoteID != uuid.Nil }

// NewQuoteBucket builds the board a quote run belongs to: the quote, and
// nothing else. There is no mode, size, language or text-source dimension,
// because a quote is a fixed map and the only meaningful comparison is against
// other runs on the same bytes (SCORING_CONCEPT §6).
func NewQuoteBucket(quoteID uuid.UUID) (Bucket, error) {
	b := Bucket{QuoteID: quoteID}
	if err := b.validate(); err != nil {
		return Bucket{}, err
	}
	return b, nil
}

// NewBucket builds the bucket a run belongs to from its raw columns. Exactly one
// of durationMs / wordCount is expected to be set — the same XOR the runs table
// enforces — and the one that matches the mode becomes the dimension.
func NewBucket(mode string, durationMs, wordCount *int32, lang, textSource string) (Bucket, error) {
	b := Bucket{Mode: mode, Lang: lang, TextSource: textSource}
	switch {
	case mode == ModeTime && durationMs != nil:
		b.Dimension = *durationMs
	case mode == ModeWords && wordCount != nil:
		b.Dimension = *wordCount
	default:
		return Bucket{}, fmt.Errorf("%w: mode %q has no matching dimension", ErrInvalidBucket, mode)
	}
	if err := b.validate(); err != nil {
		return Bucket{}, err
	}
	return b, nil
}

// Key is the bucket's identity on the wire and in the database. There are two
// shapes and they share one key space:
//
//	<mode>:<durationMs|wordCount>:<lang>:<textSource.kind>
//	time:15000:en:seeded      words:50:ru:seeded
//
//	quote:<quoteId>
//	quote:1f5f1f2c-6f0f-4d5a-9f0a-3f2a1b0c9d8e
//
// The two cannot collide and the parser never has to guess, because the first
// component of a language key is a MODE and "quote" is not one — `time` and
// `words` are the whole list (SCORING_CONCEPT §4), and a third would be a
// deliberate change to it. So the literal prefix "quote:" is a discriminator
// rather than a convention: ParseBucketKey dispatches on it before it counts
// components, and a language key can never grow into one. The reverse door is
// shut too — validate rejects TextSourceQuote as a language component, so
// "words:50:en:quote" is not a second spelling of a quote board, it is not a
// board at all.
//
// A quote key deliberately carries NOTHING else. Mode, size and language are
// not dimensions of a quote board: the quote is a fixed map with fixed bytes in
// one language, so a second component could only ever repeat what the quote id
// already determines — and any component that can disagree with the id is a way
// to split one board into two.
//
// This is the ONLY place either format is written. Nothing else — no SQL, no
// handler, no test fixture — concatenates a bucket key, because the day the
// format grows a component is the day every other producer becomes a silent
// second board. SQL matches sibling runs on the bucket's COMPONENTS instead.
func (b Bucket) Key() string {
	if b.IsQuote() {
		return quoteKeyPrefix + b.QuoteID.String()
	}
	return b.Mode + ":" + strconv.Itoa(int(b.Dimension)) + ":" + b.Lang + ":" + b.TextSource
}

// ParseBucketKey is Key's inverse: it turns a key back into its components and
// rejects anything that could not have come out of Key. The endpoints run every
// path parameter through it, so a malformed bucket is a 404 rather than an
// unbounded string reaching the index — and a malformed QUOTE key is the same
// 404 as a malformed language key, because both mean "this link names no
// board".
func ParseBucketKey(key string) (Bucket, error) {
	if rest, ok := strings.CutPrefix(key, quoteKeyPrefix); ok {
		id, err := uuid.Parse(rest)
		if err != nil {
			return Bucket{}, fmt.Errorf("%w: quote id %q is not a uuid", ErrInvalidBucket, rest)
		}
		// uuid.Parse also accepts braced, urn: and undashed spellings. They
		// name the same quote but not the same STRING, and the string is what
		// the database stores and what a client links to; accepting them would
		// hand out two keys for one board.
		if id.String() != rest {
			return Bucket{}, fmt.Errorf("%w: quote id %q is not in canonical form", ErrInvalidBucket, rest)
		}
		return NewQuoteBucket(id)
	}

	parts := strings.Split(key, ":")
	if len(parts) != 4 {
		return Bucket{}, fmt.Errorf("%w: want mode:dimension:lang:textSource or quote:<id>", ErrInvalidBucket)
	}
	dim, err := strconv.Atoi(parts[1])
	if err != nil {
		return Bucket{}, fmt.Errorf("%w: dimension %q is not a number", ErrInvalidBucket, parts[1])
	}
	b := Bucket{Mode: parts[0], Dimension: int32(dim), Lang: parts[2], TextSource: parts[3]}
	if err := b.validate(); err != nil {
		return Bucket{}, err
	}
	return b, nil
}

// validate is the single rule set behind every constructor, so a parsed bucket
// and a built one are the same thing. It also enforces that the two shapes stay
// disjoint: a bucket is a quote board or a language board, never a hybrid
// carrying half of each.
func (b Bucket) validate() error {
	if b.IsQuote() {
		if b.Mode != "" || b.Dimension != 0 || b.Lang != "" || b.TextSource != "" {
			return fmt.Errorf("%w: a quote board has no mode, size, language or text-source dimension", ErrInvalidBucket)
		}
		return nil
	}

	limit := int32(maxWordCount)
	switch b.Mode {
	case ModeTime:
		limit = maxDurationMs
	case ModeWords:
	default:
		return fmt.Errorf("%w: unknown mode %q", ErrInvalidBucket, b.Mode)
	}
	if b.Dimension <= 0 || b.Dimension > limit {
		return fmt.Errorf("%w: dimension %d out of range for mode %q", ErrInvalidBucket, b.Dimension, b.Mode)
	}
	// Inside the ingest bounds and still not a board. Checked here, in the one
	// rule set behind every constructor, so a parsed key and a projected cell
	// get the same answer: an unranked size is not a board whose page is empty,
	// it is a link that names nothing, and both doors say so.
	if !IsRankedShape(b.Mode, b.Dimension) {
		return fmt.Errorf("%w: %s runs do not rank at %d (ranked sizes: %v / %v)",
			ErrInvalidBucket, b.Mode, b.Dimension, RankedDurationsMs, RankedWordCounts)
	}
	if err := validComponent("lang", b.Lang, maxLangLen); err != nil {
		return err
	}
	if err := validComponent("textSource", b.TextSource, maxSourceLen); err != nil {
		return err
	}
	// A quote run ranks inside its quote and nowhere else (SCORING_CONCEPT §6),
	// and leaderboard_eligible_runs will never produce a row for a language
	// cell whose text source is a quote. A key naming one is therefore a key
	// naming a board that cannot exist, which is exactly what this rejects.
	if b.TextSource == TextSourceQuote {
		return fmt.Errorf("%w: quote runs rank per quote; the key is %s<id>", ErrInvalidBucket, quoteKeyPrefix)
	}
	return nil
}

// validComponent keeps the free-text components inside a charset the key format
// can round-trip: no colon, nothing that would need escaping in a URL path.
func validComponent(field, v string, maxLen int) error {
	if v == "" || len(v) > maxLen {
		return fmt.Errorf("%w: %s must be 1..%d characters", ErrInvalidBucket, field, maxLen)
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
		default:
			return fmt.Errorf("%w: %s contains %q", ErrInvalidBucket, field, r)
		}
	}
	return nil
}
