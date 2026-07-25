package quote_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/quote"
)

type metaBody struct {
	ID         uuid.UUID `json:"id"`
	Lang       string    `json:"lang"`
	UpstreamID int32     `json:"upstreamId"`
	Source     string    `json:"source"`
	Length     int32     `json:"length"`
	LenGroup   string    `json:"lenGroup"`
	TextHash   string    `json:"textHash"`
}

type listBody struct {
	Quotes     []metaBody `json:"quotes"`
	NextCursor string     `json:"nextCursor"`
}

type quoteBody struct {
	metaBody
	Text       string `json:"text"`
	Superseded bool   `json:"superseded"`
}

// /quotes is a metadata index and must stay one. The assertion is on the RAW
// keys rather than on a decoded struct: a struct silently ignores a field it
// does not model, which is exactly how a body would sneak back in.
func TestListNeverCarriesText(t *testing.T) {
	r := newRegistry(t)
	r.importLang("german", r.incoming(
		spec{upstreamID: 1, length: 40, group: quote.LenShort},
		spec{upstreamID: 2, length: 700, group: quote.LenThicc},
	))

	resp := r.get("/api/v1/quotes")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var raw struct {
		Quotes []map[string]json.RawMessage `json:"quotes"`
	}
	require.NoError(t, json.Unmarshal(readBody(t, resp), &raw))
	require.Len(t, raw.Quotes, 2)

	for _, row := range raw.Quotes {
		_, hasText := row["text"]
		assert.False(t, hasText,
			"/quotes must never return a body: it is walkable end to end, so a "+
				"text field turns it into a one-pass corpus scrape")
		assert.ElementsMatch(t,
			[]string{"id", "lang", "upstreamId", "source", "length", "lenGroup", "textHash"},
			keysOf(row))
	}
}

func TestListReportsEveryMetadataField(t *testing.T) {
	r := newRegistry(t)
	r.importLang("chinese", r.incoming(
		spec{upstreamID: 42, length: 25, group: quote.LenShort, source: "孔子"},
	))

	body := decodeInto[listBody](t, r.get("/api/v1/quotes?lang=chinese"))
	require.Len(t, body.Quotes, 1)
	got := body.Quotes[0]

	stored := r.storedQuotes()[0]
	assert.Equal(t, stored.ID, got.ID)
	assert.Equal(t, "chinese", got.Lang)
	assert.EqualValues(t, 42, got.UpstreamID)
	assert.Equal(t, "孔子", got.Source)
	assert.EqualValues(t, 25, got.Length)
	assert.Equal(t, "short", got.LenGroup)
	assert.Equal(t, stored.TextHash, got.TextHash)
	assert.Empty(t, body.NextCursor, "the last page must carry no cursor")
}

func TestListFilters(t *testing.T) {
	r := newRegistry(t)
	r.importLang("german", r.incoming(
		spec{upstreamID: 1, length: 40, group: quote.LenShort},
		spec{upstreamID: 2, length: 400, group: quote.LenLong},
	))
	r.importLang("russian", r.incoming(
		spec{upstreamID: 1, length: 40, group: quote.LenShort},
	))

	t.Run("by language", func(t *testing.T) {
		body := decodeInto[listBody](t, r.get("/api/v1/quotes?lang=german"))
		require.Len(t, body.Quotes, 2)
		for _, q := range body.Quotes {
			assert.Equal(t, "german", q.Lang)
		}
	})

	t.Run("by group", func(t *testing.T) {
		body := decodeInto[listBody](t, r.get("/api/v1/quotes?group=short"))
		require.Len(t, body.Quotes, 2)
		for _, q := range body.Quotes {
			assert.Equal(t, "short", q.LenGroup)
		}
	})

	t.Run("both", func(t *testing.T) {
		body := decodeInto[listBody](t, r.get("/api/v1/quotes?lang=german&group=long"))
		require.Len(t, body.Quotes, 1)
		assert.EqualValues(t, 2, body.Quotes[0].UpstreamID)
	})

	t.Run("an unknown group is rejected, not ignored", func(t *testing.T) {
		for _, bad := range []string{"enormous", "0", "SHORT", "-1"} {
			resp := r.get("/api/v1/quotes?group=" + bad)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode,
				"group=%q must be a 400: silently dropping the filter would hand "+
					"back the wrong quotes and look like a corpus bug", bad)
		}
	})

	t.Run("an unknown language is an empty page, not an error", func(t *testing.T) {
		body := decodeInto[listBody](t, r.get("/api/v1/quotes?lang=klingon"))
		assert.Empty(t, body.Quotes)
	})
}

func TestListIsEmptyNotNull(t *testing.T) {
	r := newRegistry(t)
	resp := r.get("/api/v1/quotes")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, `{"quotes":[]}`, string(readBody(t, resp)),
		"an empty page must be [] so a client can render it without a null check")
}

func TestListLimitIsClamped(t *testing.T) {
	r := newRegistry(t)
	var specs []spec
	for i := 1; i <= 5; i++ {
		specs = append(specs, spec{upstreamID: int32(i), length: 40, group: quote.LenShort})
	}
	r.importLang("german", r.incoming(specs...))

	for _, q := range []string{"?limit=0", "?limit=-5", "?limit=abc", "?limit=9999", ""} {
		body := decodeInto[listBody](t, r.get("/api/v1/quotes"+q))
		assert.Len(t, body.Quotes, 5, "limit %q must fall back inside [1,200]", q)
	}
}

func TestListRejectsABadCursor(t *testing.T) {
	r := newRegistry(t)
	for _, bad := range []string{"!!!", "Zm9v", "Zm9vOjE6bm90LWEtdXVpZA"} {
		resp := r.get("/api/v1/quotes?cursor=" + bad)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "cursor %q", bad)
	}
}

// Walking the whole corpus with the cursor must return every quote exactly
// once. The concurrent insert is the real test: the row lands BEFORE the
// cursor's position in the total order, which is precisely the case an
// offset-based pager gets wrong by shifting every later page.
func TestPaginationIsStableUnderAConcurrentInsert(t *testing.T) {
	r := newRegistry(t)
	var specs []spec
	for i := 1; i <= 25; i++ {
		specs = append(specs, spec{upstreamID: int32(i), length: 40, group: quote.LenShort})
	}
	r.importLang("german", r.incoming(specs...))

	want := make(map[uuid.UUID]bool, 25)
	for _, row := range r.storedQuotes() {
		want[row.ID] = false
	}
	require.Len(t, want, 25)

	seen := make(map[uuid.UUID]int, 25)
	cursor := ""
	inserted := false
	for page := 0; ; page++ {
		require.Less(t, page, 20, "the walk is not terminating")

		url := "/api/v1/quotes?limit=5"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		body := decodeInto[listBody](t, r.get(url))
		for _, q := range body.Quotes {
			seen[q.ID]++
		}

		// After the first page, plant a row that sorts before everything the
		// walk has already returned.
		if !inserted {
			r.insertBefore("german", quote.LenShort)
			inserted = true
		}

		if body.NextCursor == "" {
			break
		}
		cursor = body.NextCursor
	}

	for id := range want {
		assert.Equal(t, 1, seen[id], "quote %s appeared %d times in the walk", id, seen[id])
	}
	for id, n := range seen {
		assert.Equal(t, 1, n, "quote %s appeared %d times", id, n)
		if _, original := want[id]; !original {
			assert.Fail(t, "a row inserted before the cursor must not appear in the walk", id.String())
		}
	}
}

// /quotes/random is what starting a quote run calls: one quote, with its text,
// never a retired one, honouring both filters.
func TestRandomRespectsFilters(t *testing.T) {
	r := newRegistry(t)
	r.importLang("german", r.incoming(
		spec{upstreamID: 1, length: 40, group: quote.LenShort},
		spec{upstreamID: 2, length: 400, group: quote.LenLong},
	))
	r.importLang("russian", r.incoming(
		spec{upstreamID: 1, length: 40, group: quote.LenShort},
	))

	t.Run("returns the text", func(t *testing.T) {
		body := decodeInto[quoteBody](t, r.get("/api/v1/quotes/random?lang=german&group=long"))
		assert.EqualValues(t, 2, body.UpstreamID)
		assert.Equal(t, "german", body.Lang)
		assert.Equal(t, "long", body.LenGroup)
		assert.NotEmpty(t, body.Text, "a draw with no text cannot start a run")
		assert.EqualValues(t, len([]rune(body.Text)), body.Length)
		assert.False(t, body.Superseded)
	})

	t.Run("honours the language", func(t *testing.T) {
		for range 15 {
			body := decodeInto[quoteBody](t, r.get("/api/v1/quotes/random?lang=russian"))
			assert.Equal(t, "russian", body.Lang)
		}
	})

	t.Run("an unknown group is rejected", func(t *testing.T) {
		resp := r.get("/api/v1/quotes/random?group=enormous")
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("a filter nothing matches is 404, not an empty body", func(t *testing.T) {
		resp := r.get("/api/v1/quotes/random?lang=german&group=thicc")
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

// A LIMIT 1 under a constant order returns the same quote forever. Over enough
// draws on a small corpus that is what this catches.
func TestRandomActuallyVaries(t *testing.T) {
	r := newRegistry(t)
	var specs []spec
	for i := 1; i <= 6; i++ {
		specs = append(specs, spec{upstreamID: int32(i), length: 40, group: quote.LenShort})
	}
	r.importLang("german", r.incoming(specs...))

	seen := make(map[uuid.UUID]int)
	for range 60 {
		body := decodeInto[quoteBody](t, r.get("/api/v1/quotes/random?lang=german&group=short"))
		seen[body.ID]++
	}
	// Six quotes, sixty draws: seeing one id every time is a fixed order, not
	// bad luck (probability 6 * (1/6)^60).
	assert.Greater(t, len(seen), 1,
		"every draw returned the same quote — the selection is not random")
	t.Logf("60 draws over 6 quotes hit %d distinct ids", len(seen))
}

// A retired quote is invisible to browsing and to the draw, and permanently
// visible by id. Anything else makes every run played on it unwatchable.
func TestSupersededQuotesAreByIDOnly(t *testing.T) {
	r := newRegistry(t)
	r.importLang("german", r.incoming(spec{upstreamID: 1, length: 40, group: quote.LenShort}))
	old := r.storedQuotes()[0]

	r.importLang("german", r.incoming(spec{upstreamID: 1, text: "the corrected text of quote one"}))

	t.Run("by id, forever", func(t *testing.T) {
		body := decodeInto[quoteBody](t, r.get("/api/v1/quotes/"+old.ID.String()))
		assert.True(t, body.Superseded)
		assert.Equal(t, old.Text, body.Text, "the retired bytes must be served verbatim")
		assert.Equal(t, old.TextHash, body.TextHash)
	})

	t.Run("absent from browsing", func(t *testing.T) {
		body := decodeInto[listBody](t, r.get("/api/v1/quotes?lang=german"))
		require.Len(t, body.Quotes, 1)
		assert.NotEqual(t, old.ID, body.Quotes[0].ID)
	})

	t.Run("never drawn", func(t *testing.T) {
		for range 25 {
			body := decodeInto[quoteBody](t, r.get("/api/v1/quotes/random?lang=german"))
			assert.NotEqual(t, old.ID, body.ID, "a retired quote must never start a run")
			assert.False(t, body.Superseded)
		}
	})
}

func TestByIDRejectsJunk(t *testing.T) {
	r := newRegistry(t)
	for _, path := range []string{
		"/api/v1/quotes/" + uuid.New().String(),
		"/api/v1/quotes/not-a-uuid",
		"/api/v1/quotes/1",
	} {
		resp := r.get(path)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, path)
	}
}

// The corpus is public: a guest picking a text to type has no account yet, so
// none of these routes may need one. There is no session in this harness at
// all, which is the assertion.
func TestQuotesAreReadableWithoutASession(t *testing.T) {
	r := newRegistry(t)
	r.importLang("german", r.incoming(spec{upstreamID: 1, length: 40, group: quote.LenShort}))
	id := r.storedQuotes()[0].ID

	for _, path := range []string{
		"/api/v1/quotes",
		"/api/v1/quotes/random",
		"/api/v1/quotes/" + id.String(),
	} {
		assert.Equal(t, http.StatusOK, r.get(path).StatusCode, path)
	}
}

// insertBefore plants a published row whose (lang, len_group, id) sorts before
// every uuid Postgres generates, so a paging walk that is not keyset-stable
// will either skip a row or repeat one.
func (r *registry) insertBefore(lang string, group quote.LenGroup) uuid.UUID {
	r.t.Helper()
	id := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	text := "planted mid-walk, sorts before every other row"
	hash, err := quote.HashText(r.core, text)
	require.NoError(r.t, err)

	_, err = r.pool.Exec(context.Background(), `
		INSERT INTO quotes (id, lang, upstream_id, text, source, length, len_group, text_hash)
		VALUES ($1, $2, 9999, $3, 'concurrent', char_length($3), $4, $5)`,
		id, lang, text, int16(group), hash)
	require.NoError(r.t, err)
	return id
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
