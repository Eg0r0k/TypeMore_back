package leaderboard

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Ranked modes. Zen and custom text are unranked by design (SCORING_CONCEPT §2)
// and never reach a bucket at all.
const (
	ModeTime  = "time"
	ModeWords = "words"
)

// TextSourceSeeded is the only text origin that scores today: text generated
// from a server-issued seed. Quotes get their own per-quote boards later
// (SCORING_CONCEPT §6), which is why the kind is part of the key rather than an
// assumption baked into it.
const TextSourceSeeded = "seeded"

// Dimension bounds, mirroring the ingest validator's (docs/RUNS.md): a bucket
// can only ever describe a run that was allowed in.
const (
	maxDurationMs = 3_600_000
	maxWordCount  = 10_000
	maxLangLen    = 32
	maxSourceLen  = 16
)

// ErrInvalidBucket is returned for a bucket that cannot name a board — an
// unknown mode, a dimension outside the ingest bounds, or a component carrying
// characters the key format cannot round-trip.
var ErrInvalidBucket = errors.New("leaderboard: invalid bucket")

// Bucket names one leaderboard: a mode at one size, in one language, over one
// kind of text. Mods are deliberately NOT part of it — they multiply the score
// (SCORING_CONCEPT §2) instead of splitting the board, so a punctuation run and
// a plain one compete directly.
type Bucket struct {
	// Mode is ModeTime or ModeWords.
	Mode string
	// Dimension is the mode's size: milliseconds for time modes, words for word
	// modes. The two never collide in a key because Mode precedes it.
	Dimension int32
	// Lang is the dictionary language the text was generated from.
	Lang string
	// TextSource is the textSource.kind the run declared (TextSourceSeeded).
	TextSource string
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

// Key is the bucket's identity on the wire and in the database:
//
//	<mode>:<durationMs|wordCount>:<lang>:<textSource.kind>
//	time:15000:en:seeded      words:50:ru:seeded
//
// This is the ONLY place the format is written. Nothing else — no SQL, no
// handler, no test fixture — concatenates a bucket key, because the day the
// format grows a component is the day every other producer becomes a silent
// second board. SQL matches sibling runs on the bucket's COMPONENTS instead.
func (b Bucket) Key() string {
	return b.Mode + ":" + strconv.Itoa(int(b.Dimension)) + ":" + b.Lang + ":" + b.TextSource
}

// ParseBucketKey is Key's inverse: it turns a key back into its components and
// rejects anything that could not have come out of Key. The endpoints run every
// path parameter through it, so a malformed bucket is a 404 rather than an
// unbounded string reaching the index.
func ParseBucketKey(key string) (Bucket, error) {
	parts := strings.Split(key, ":")
	if len(parts) != 4 {
		return Bucket{}, fmt.Errorf("%w: want mode:dimension:lang:textSource", ErrInvalidBucket)
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

// validate is the single rule set behind both constructors, so a parsed bucket
// and a built one are the same thing.
func (b Bucket) validate() error {
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
	if err := validComponent("lang", b.Lang, maxLangLen); err != nil {
		return err
	}
	return validComponent("textSource", b.TextSource, maxSourceLen)
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
