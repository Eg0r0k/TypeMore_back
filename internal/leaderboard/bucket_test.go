package leaderboard_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/leaderboard"
)

// The key format is the leaderboard's identity: it names rows in the database,
// paths in the API, and links people paste at each other. Every shape it must
// produce is pinned here, because the day it changes silently is the day every
// stored bucket_key points at a board nobody can reach.
func TestBucketKeyFormat(t *testing.T) {
	cases := []struct {
		name       string
		mode       string
		durationMs *int32
		wordCount  *int32
		lang       string
		source     string
		want       string
	}{
		{"time 15s", leaderboard.ModeTime, new(int32(15000)), nil, "en", leaderboard.TextSourceSeeded, "time:15000:en:seeded"},
		{"time 30s", leaderboard.ModeTime, new(int32(30000)), nil, "en", leaderboard.TextSourceSeeded, "time:30000:en:seeded"},
		{"time 60s", leaderboard.ModeTime, new(int32(60000)), nil, "ru", leaderboard.TextSourceSeeded, "time:60000:ru:seeded"},
		{"words 25", leaderboard.ModeWords, nil, new(int32(25)), "en", leaderboard.TextSourceSeeded, "words:25:en:seeded"},
		{"words 50", leaderboard.ModeWords, nil, new(int32(50)), "german", leaderboard.TextSourceSeeded, "words:50:german:seeded"},
		{"words 100", leaderboard.ModeWords, nil, new(int32(100)), "css_code", leaderboard.TextSourceSeeded, "words:100:css_code:seeded"},
		// A future text source is a different board, not the same one with a
		// footnote — the kind is IN the key so quotes cannot land in the
		// generated-text ranking (SCORING_CONCEPT §6).
		{"another source", leaderboard.ModeWords, nil, new(int32(50)), "en", "quote", "words:50:en:quote"},
		// The dimension is whichever column the mode uses; a stray value in the
		// other one is ignored rather than smuggled into the key.
		{"both columns set", leaderboard.ModeTime, new(int32(15000)), new(int32(50)), "en", leaderboard.TextSourceSeeded, "time:15000:en:seeded"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := leaderboard.NewBucket(tc.mode, tc.durationMs, tc.wordCount, tc.lang, tc.source)
			require.NoError(t, err)
			assert.Equal(t, tc.want, b.Key())

			// Round-trip: the endpoints parse what the projection wrote, so a
			// key that cannot be read back is a board that cannot be served.
			parsed, err := leaderboard.ParseBucketKey(tc.want)
			require.NoError(t, err)
			assert.Equal(t, b, parsed)
			assert.Equal(t, tc.want, parsed.Key())
		})
	}
}

// A time bucket and a word bucket can carry the same number. The mode prefix is
// what keeps "60 words" and "60 milliseconds" from being the same board.
func TestBucketKeysAreDisjointAcrossModes(t *testing.T) {
	timeBucket, err := leaderboard.NewBucket(leaderboard.ModeTime, new(int32(60)), nil, "en", leaderboard.TextSourceSeeded)
	require.NoError(t, err)
	wordBucket, err := leaderboard.NewBucket(leaderboard.ModeWords, nil, new(int32(60)), "en", leaderboard.TextSourceSeeded)
	require.NoError(t, err)
	assert.NotEqual(t, timeBucket.Key(), wordBucket.Key())
}

func TestBucketRejectsUnrankableShapes(t *testing.T) {
	cases := []struct {
		name       string
		mode       string
		durationMs *int32
		wordCount  *int32
	}{
		{"unknown mode", "zen", new(int32(15000)), nil},
		{"time without a duration", leaderboard.ModeTime, nil, new(int32(50))},
		{"words without a count", leaderboard.ModeWords, new(int32(15000)), nil},
		{"no dimension at all", leaderboard.ModeTime, nil, nil},
		{"zero duration", leaderboard.ModeTime, new(int32(0)), nil},
		{"negative words", leaderboard.ModeWords, nil, new(int32(-1))},
		{"duration past the ingest ceiling", leaderboard.ModeTime, new(int32(3_600_001)), nil},
		{"word count past the ingest ceiling", leaderboard.ModeWords, nil, new(int32(10_001))},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := leaderboard.NewBucket(tc.mode, tc.durationMs, tc.wordCount, "en", leaderboard.TextSourceSeeded)
			require.ErrorIs(t, err, leaderboard.ErrInvalidBucket)
		})
	}
}

// The bucket is a URL path segment, so the parser is an input boundary: whatever
// it accepts reaches an index lookup.
func TestParseBucketKeyRejectsJunk(t *testing.T) {
	cases := []struct{ name, key string }{
		{"empty", ""},
		{"too few components", "time:15000:en"},
		{"too many components", "time:15000:en:seeded:extra"},
		{"non-numeric dimension", "time:fifteen:en:seeded"},
		{"unknown mode", "zen:15000:en:seeded"},
		{"empty lang", "time:15000::seeded"},
		{"empty source", "time:15000:en:"},
		{"lang with a slash", "time:15000:en/x:seeded"},
		{"lang with a wildcard", "time:15000:%:seeded"},
		{"lang with a space", "time:15000:en us:seeded"},
		{"sql-ish lang", "time:15000:en';DROP:seeded"},
		{"oversized lang", "time:15000:" + string(make([]byte, 0, 40)) + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:seeded"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := leaderboard.ParseBucketKey(tc.key)
			require.ErrorIs(t, err, leaderboard.ErrInvalidBucket)
		})
	}
}
