package replay

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestServer builds the real registry (seeded through the real goja bundle)
// behind the same routes main.go mounts.
func newTestServer(t *testing.T) (*httptest.Server, *Registry, *Core) {
	t.Helper()

	core, err := NewCore(0)
	require.NoError(t, err)
	reg, err := NewRegistry(core)
	require.NoError(t, err)
	svc, err := NewDictionaryService(reg)
	require.NoError(t, err)

	r := chi.NewRouter()
	r.Mount("/api/v1/dictionaries", svc.Routes())
	r.Mount("/static/dictionaries", svc.StaticRoutes())

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, reg, core
}

// get issues a GET with no automatic decompression, so the test sees exactly
// what the handler wrote on the wire.
func get(t *testing.T, url string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, http.NoBody)
	require.NoError(t, err)
	// Go's transport adds Accept-Encoding: gzip and transparently decodes unless
	// the caller sets the header itself. Setting identity keeps the default
	// case honest; individual tests override it.
	req.Header.Set("Accept-Encoding", "identity")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func readBody(t *testing.T, res *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return b
}

// The catalogue must list every dictionary the registry seeded — a language
// missing here is a language no client can ever discover.
func TestCatalogueListsEverySeededDictionary(t *testing.T) {
	srv, reg, core := newTestServer(t)

	res := get(t, srv.URL+"/api/v1/dictionaries", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	assert.Equal(t, "application/json; charset=utf-8", res.Header.Get("Content-Type"))

	var got []CatalogueEntry
	require.NoError(t, json.Unmarshal(readBody(t, res), &got))
	require.Equal(t, reg.Catalogue(), got)

	// Every embedded file is represented, with the metadata a client needs to
	// pick a language and fetch its body.
	files, err := dictFS.ReadDir(dictsDir)
	require.NoError(t, err)
	require.Len(t, got, len(files), "one catalogue row per embedded dictionary")

	byLang := make(map[string]CatalogueEntry, len(got))
	for _, e := range got {
		assert.NotEmpty(t, e.Name, "%s: name", e.Lang)
		assert.NotEmpty(t, e.DictHash, "%s: dictHash", e.Lang)
		assert.Positive(t, e.WordCount, "%s: wordCount", e.Lang)
		assert.Positive(t, e.Bytes, "%s: bytes", e.Lang)
		byLang[e.Lang] = e
	}

	for _, f := range files {
		lang := f.Name()[:len(f.Name())-len(".json")]
		e, ok := byLang[lang]
		require.True(t, ok, "dictionary %q missing from the catalogue", lang)

		// The advertised numbers describe the real file, not an estimate.
		raw, err := dictFS.ReadFile(path.Join(dictsDir, f.Name()))
		require.NoError(t, err)
		var doc dictDoc
		require.NoError(t, json.Unmarshal(raw, &doc))

		assert.Equal(t, doc.Name, e.Name, "%s: name", lang)
		assert.Len(t, doc.Words, e.WordCount, "%s: wordCount", lang)
		assert.Equal(t, len(raw), e.Bytes, "%s: bytes", lang)

		hash, err := core.DictVersion(doc.Words)
		require.NoError(t, err)
		assert.Equal(t, hash, e.DictHash, "%s: dictHash must come from the core bundle", lang)
	}
}

// publishedHashes freezes every dict_hash this server has published. A run
// stores the dict_hash it was generated from and must stay replayable forever,
// so editing a shipped word list is a breaking change, not a content tweak:
// publish a NEW language file instead. This test is the tripwire — if it fails,
// revert the dictionary edit; never update the golden value. See
// docs/DICTIONARIES.md.
var publishedHashes = map[string]string{
	"arabian":             "09fa6ceb",
	"chinese":             "2557d6b5",
	"css_code":            "55ccd317",
	"english":             "be99aa1a",
	"french":              "3a153572",
	"german":              "804728e8",
	"japanese":            "92ed2422",
	"russian":             "f5aacfd2",
	"russian_empire":      "be593606",
	"traditional_chinese": "58c5a72d",
}

func TestPublishedHashesAreImmutable(t *testing.T) {
	core, err := NewCore(0)
	require.NoError(t, err)
	reg, err := NewRegistry(core)
	require.NoError(t, err)

	for _, e := range reg.Catalogue() {
		want, published := publishedHashes[e.Lang]
		if !published {
			continue // a newly added language; add it to the golden map.
		}
		assert.Equal(t, want, e.DictHash,
			"dictionary %q changed: every run recorded against %s is now unreplayable. "+
				"Publish a new language file instead of editing this one.", e.Lang, want)
	}

	// Removing a published dictionary is the same breakage by another route:
	// its hash must still resolve.
	for lang, hash := range publishedHashes {
		_, ok := reg.Body(hash)
		assert.True(t, ok, "published dictionary %q (%s) was removed; old runs still reference it", lang, hash)
	}
}

// The contract of content addressing: whatever the body endpoint returns for a
// hash must be a dictionary whose word list hashes back to that same hash.
func TestBodyIsTheContentItsHashAddresses(t *testing.T) {
	srv, reg, core := newTestServer(t)

	for _, e := range reg.Catalogue() {
		t.Run(e.Lang, func(t *testing.T) {
			res := get(t, srv.URL+"/static/dictionaries/"+e.DictHash+".json", nil)
			require.Equal(t, http.StatusOK, res.StatusCode)

			body := readBody(t, res)

			// Round-trip: the FNV-1a of the returned word list IS the requested
			// hash, computed by the same core the client uses.
			var doc dictDoc
			require.NoError(t, json.Unmarshal(body, &doc))
			hash, err := core.DictVersion(doc.Words)
			require.NoError(t, err)
			assert.Equal(t, e.DictHash, hash)

			// Exact bytes: byte-for-byte the stored file, and exactly the length
			// the catalogue advertised.
			stored, ok := reg.Body(e.DictHash)
			require.True(t, ok)
			assert.Equal(t, stored, body)
			assert.Len(t, body, e.Bytes)
			assert.Equal(t, strconv.Itoa(e.Bytes), res.Header.Get("Content-Length"))
		})
	}
}

// A hash nobody published resolves to nothing — never to "the current version"
// of some language, which would silently replay a run against different words.
func TestUnknownHashIs404(t *testing.T) {
	srv, _, _ := newTestServer(t)

	for _, hash := range []string{"deadbeef", "00000000", "not-a-hash"} {
		res := get(t, srv.URL+"/static/dictionaries/"+hash+".json", nil)
		require.Equal(t, http.StatusNotFound, res.StatusCode, "hash %q", hash)

		var body struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		require.NoError(t, json.Unmarshal(readBody(t, res), &body))
		assert.Equal(t, "not_found", body.Error)
		assert.NotEmpty(t, body.Message)
	}
}

// Immutability is the whole reason bodies are hash-addressed; the headers have
// to say so, and the ETag has to be the hash itself.
func TestBodyServedImmutable(t *testing.T) {
	srv, reg, _ := newTestServer(t)
	e := reg.Catalogue()[0]
	url := srv.URL + "/static/dictionaries/" + e.DictHash + ".json"

	res := get(t, url, nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	assert.Equal(t, "public, max-age=31536000, immutable", res.Header.Get("Cache-Control"))
	assert.Equal(t, `"`+e.DictHash+`"`, res.Header.Get("ETag"))
	assert.Equal(t, "Accept-Encoding", res.Header.Get("Vary"))

	// Revalidation costs no body.
	res = get(t, url, map[string]string{"If-None-Match": `"` + e.DictHash + `"`})
	require.Equal(t, http.StatusNotModified, res.StatusCode)
	assert.Empty(t, readBody(t, res))
	assert.Equal(t, "public, max-age=31536000, immutable", res.Header.Get("Cache-Control"))
}

// The catalogue is explicitly NOT immutable — a new language must become
// visible — but it still revalidates cheaply.
func TestCatalogueIsRevalidatableNotImmutable(t *testing.T) {
	srv, _, _ := newTestServer(t)
	url := srv.URL + "/api/v1/dictionaries"

	res := get(t, url, nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	assert.Equal(t, "public, max-age=60", res.Header.Get("Cache-Control"))
	assert.NotContains(t, res.Header.Get("Cache-Control"), "immutable")
	etag := res.Header.Get("ETag")
	require.NotEmpty(t, etag)

	res = get(t, url, map[string]string{"If-None-Match": etag})
	require.Equal(t, http.StatusNotModified, res.StatusCode)
	assert.Empty(t, readBody(t, res))
}

// Pre-gzipped bytes are served as-is to clients that accept them, and never to
// clients that refuse them.
func TestGzipNegotiation(t *testing.T) {
	srv, reg, _ := newTestServer(t)
	e := reg.Catalogue()[0]
	url := srv.URL + "/static/dictionaries/" + e.DictHash + ".json"
	stored, ok := reg.Body(e.DictHash)
	require.True(t, ok)

	res := get(t, url, map[string]string{"Accept-Encoding": "gzip"})
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, "gzip", res.Header.Get("Content-Encoding"))
	compressed := readBody(t, res)
	assert.Less(t, len(compressed), len(stored), "gzip must actually be smaller")

	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	require.NoError(t, err)
	inflated, err := io.ReadAll(zr)
	require.NoError(t, err)
	assert.Equal(t, stored, inflated, "gzip must decode to the exact stored bytes")

	// An explicit refusal is honoured.
	res = get(t, url, map[string]string{"Accept-Encoding": "gzip;q=0"})
	require.Equal(t, http.StatusOK, res.StatusCode)
	assert.Empty(t, res.Header.Get("Content-Encoding"))
	assert.Equal(t, stored, readBody(t, res))
}
