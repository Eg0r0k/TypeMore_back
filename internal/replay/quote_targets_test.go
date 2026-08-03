package replay

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// THE TEXT IS CHECKED BEFORE THE SCORE.
//
// A quote run's whole judgement rests on one thing happening first: the server
// has to arrive at the SAME TARGET WORDS the browser typed against. Everything
// after that — the fold, the metrics, the score — is the seeded pipeline
// unchanged, and it can only agree if the targets already do.
//
// The rule that produces those targets is small enough to look re-writable in
// Go, which is exactly why it must not be: every line break becomes "\n ", then
// the text is split on U+0020 with empty tokens dropped, and NOTHING ELSE is
// touched — not a typographic quote, not a no-break space, not an em dash, not
// a tab. A Go tokeniser that "tidied" any of those would disagree with the
// client on some texts and on no others, so every run on those quotes would be
// refused while the suite stayed green.
//
// The expectations below are LITERALS, mirroring the frontend's own pin
// (`src/__tests__/core/quote-source.test.ts`, "quote targets are the text,
// split on spaces"). They do not ask the bundle what it thinks the answer is;
// they state it. That is what makes this a cross-repo contract rather than the
// bundle agreeing with itself.
func TestQuoteTargetsComeFromTheBundleAndNormaliseNothing(t *testing.T) {
	core, err := NewCore(0)
	require.NoError(t, err)
	ctx := context.Background()

	cases := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "splits on spaces and nothing else",
			text: "the quick brown fox",
			want: []string{"the", "quick", "brown", "fox"},
		},
		{
			name: "runs of spaces and the edges yield no empty target",
			text: "  double  spaced  text  ",
			want: []string{"double", "spaced", "text"},
		},
		{
			name: "a newline ends its token; a tab opens the next",
			text: "a\nb c\td",
			want: []string{"a\n", "b", "c\td"},
		},
		{
			name: "a doubled newline is a blank line of its own",
			text: "a\n\nb",
			want: []string{"a\n", "\n", "b"},
		},
		{
			name: "spaces around a newline are absorbed",
			text: "a  \n  b",
			want: []string{"a\n", "b"},
		},
		// The three the task names by hand. Each of these is a character an
		// over-helpful server would "fix" — and each fix would be a mismatch on
		// EVERY run of the quote that contains it.
		{
			name: "typographic quotes and apostrophes survive verbatim",
			text: "“It’s” fine",
			want: []string{"“It’s”", "fine"},
		},
		{
			name: "a no-break space is NOT a separator and is NOT folded to U+0020",
			text: "10 km per hour",
			want: []string{"10 km", "per", "hour"},
		},
		{
			name: "dashes stay the dash they are",
			text: "a — b – c - d",
			want: []string{"a", "—", "b", "–", "c", "-", "d"},
		},
		{
			name: "a narrow no-break space is likewise inert",
			text: "1 000 ёжик",
			want: []string{"1 000", "ёжик"},
		},
	}

	// U+2026 is the ONE character the tokeniser rewrites, and the exception is
	// deliberate: everything above is a character a player CAN type and a server
	// must not tidy, while an ellipsis is a character no mainstream layout
	// produces at all. A target containing one is not a hard target, it is an
	// unwinnable one — see `expandEllipsis` (words.ts) for why the answer is
	// three periods rather than a normalize.ts equivalence.
	//
	// Position is what these cases are about: the rewrite runs per TOKEN, after
	// the split, so the token count and every boundary stay exactly where they
	// were with the ellipsis left alone.
	cases = append(cases, []struct {
		name string
		text string
		want []string
	}{
		{
			name: "an ellipsis opening a word becomes three periods, in place",
			text: "…и снова",
			want: []string{"...и", "снова"},
		},
		{
			name: "an ellipsis inside a word becomes three periods, in place",
			text: "หา…ไม่ ok",
			want: []string{"หา...ไม่", "ok"},
		},
		{
			name: "an ellipsis closing a word becomes three periods, in place",
			text: "калі… нова…",
			want: []string{"калі...", "нова..."},
		},
		{
			name: "a lone ellipsis is a whole token of three periods",
			text: "a … b",
			want: []string{"a", "...", "b"},
		},
		{
			name: "two ellipses in a row expand independently",
			text: "wait…… what",
			want: []string{"wait......", "what"},
		},
		{
			name: "the rewrite is per token, so a separator never moves",
			text: "…\n… x",
			want: []string{"...\n", "...", "x"},
		},
	}...)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := core.QuoteTargets(ctx, tc.text)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// The registry's own quote, end to end through the same call. The golden vector
// carries the bytes a real run was played on, and its double space is the
// interesting part: it collapses to ONE separator, so the server's target count
// has to match the client's rather than gaining an empty word in the middle.
func TestTheQuoteVectorsTextTokenisesToTheTargetsItWasPlayedOn(t *testing.T) {
	core, err := NewCore(0)
	require.NoError(t, err)

	var text, wantHash string
	for _, v := range loadVectors(t) {
		if v.Quote != nil {
			text, wantHash = v.Quote.Text, v.Quote.Hash
			break
		}
	}
	require.NotEmpty(t, text, "no quote vector: the text-resolution path is unpinned")

	targets, err := core.QuoteTargets(context.Background(), text)
	require.NoError(t, err)
	assert.NotEmpty(t, targets)
	for _, target := range targets {
		assert.NotEmpty(t, target, "an empty target is not a typeable word")
		assert.NotContains(t, target, " ",
			"a target may not contain the separator it was split on")
	}
	// The hash the run carries is the hash OF THESE BYTES — the same identity
	// the resolver checks before the core is ever entered.
	hash, err := core.DictVersion([]string{text})
	require.NoError(t, err)
	assert.Equal(t, wantHash, hash)
}
