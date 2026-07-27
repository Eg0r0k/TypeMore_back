package replay

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dop251/goja"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/perf"
)

// newTestServer builds the real registry (seeded through the real goja bundle)
// behind the same routes main.go mounts.
func newTestServer(t *testing.T) (*httptest.Server, *Registry, *Core) {
	t.Helper()

	core, reg := sharedDicts(t)
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

		// The name is the catalogue's, not the file's: displayNames owns it.
		assert.Equal(t, displayNames[lang], e.Name, "%s: name", lang)
		assert.Len(t, doc.Words, e.WordCount, "%s: wordCount", lang)
		assert.Equal(t, len(raw), e.Bytes, "%s: bytes", lang)

		hash, err := core.DictVersion(doc.Words)
		require.NoError(t, err)
		assert.Equal(t, hash, e.DictHash, "%s: dictHash must come from the core bundle", lang)
	}
}

// The catalogue's `name` is a human display name owned by the server, and the
// key is the only thing that travels. Two separate failures hide behind one
// symptom here, so both are asserted: a picker that renders `lang` shows the
// user "code_css", and a catalogue that derives the name from the key shows the
// user "Code Css". Neither is a name, and both looked like working code.
func TestCatalogueNamesAreDisplayNamesNotKeys(t *testing.T) {
	_, reg, _ := newTestServer(t)

	for _, e := range reg.Catalogue() {
		assert.NotEqual(t, e.Lang, e.Name,
			"%s: the catalogue is publishing the raw key as its display name", e.Lang)
	}

	byLang := make(map[string]string)
	for _, e := range reg.Catalogue() {
		byLang[e.Lang] = e.Name
	}
	// Spot the entries whose name no transformation of the key could produce.
	assert.Equal(t, "CSS (code)", byLang["code_css"])
	assert.Equal(t, "Arabic", byLang["arabian"])
	assert.Equal(t, "Russian (pre-reform)", byLang["russian_empire"])
	assert.Equal(t, "Chinese (simplified)", byLang["chinese"])
}

// A dictionary with no row in displayNames must fail loudly at construction.
// The tempting alternative — fall back to the key — is not a lesser evil, it is
// precisely the bug: it ships a catalogue that looks fine and puts a raw key in
// front of a user. Vendoring a dictionary and forgetting to name it has to be
// impossible to miss, so it is an error and not a default.
func TestADictionaryWithNoDisplayNameIsAStartupError(t *testing.T) {
	name, err := displayName("code_rust") // vendored in this test's imagination only
	require.Error(t, err)
	assert.Empty(t, name, "a failed lookup must not hand back something renderable")
	assert.Contains(t, err.Error(), "code_rust")
	assert.Contains(t, err.Error(), "displayNames")

	// Every embedded dictionary does have one — that is the invariant the error
	// above protects, checked against the real embedded set rather than assumed.
	files, err := dictFS.ReadDir(dictsDir)
	require.NoError(t, err)
	for _, f := range files {
		lang := strings.TrimSuffix(f.Name(), ".json")
		got, err := displayName(lang)
		require.NoError(t, err, "embedded dictionary %q has no display name", lang)
		assert.NotEmpty(t, got)
	}
}

// publishedHashes freezes every dict_hash this server has published. A run
// stores the dict_hash it was generated from and must stay replayable forever,
// so editing a shipped word list is a breaking change, not a content tweak:
// publish a NEW language file instead. This test is the tripwire — if it fails,
// revert the dictionary edit; never update the golden value. See
// docs/DICTIONARIES.md.
var publishedHashes = map[string]string{
	"afrikaans":                   "07334ba7",
	"afrikaans_10k":               "a524e44c",
	"afrikaans_1k":                "e6c4531a",
	"albanian":                    "45288b96",
	"albanian_1k":                 "04f75b03",
	"amharic":                     "9d76ef2d",
	"amharic_1k":                  "164cffe5",
	"amharic_5k":                  "314c6a49",
	"arabian":                     "09fa6ceb",
	"arabian_10k":                 "636ecdff",
	"arabian_egypt":               "c41dcb4a",
	"arabian_egypt_1k":            "08eb339d",
	"arabian_morocco":             "1c52b9e2",
	"armenian":                    "4d6c680c",
	"armenian_1k":                 "597473d2",
	"armenian_western":            "e6c58a55",
	"armenian_western_1k":         "cd965456",
	"azerbaijani":                 "d02cfc24",
	"azerbaijani_1k":              "b3b745b1",
	"bangla":                      "983087a2",
	"bangla_10k":                  "13535aa5",
	"bangla_letters":              "22510cab",
	"bashkir":                     "1d387f09",
	"belarusian":                  "c612c4d5",
	"belarusian_100k":             "29193375",
	"belarusian_10k":              "430cc03a",
	"belarusian_1k":               "64a124f0",
	"belarusian_25k":              "fe137bc0",
	"belarusian_50k":              "3031301d",
	"belarusian_5k":               "15f298fc",
	"belarusian_lacinka":          "be6369a7",
	"belarusian_lacinka_1k":       "463d37dd",
	"bemba":                       "788c5bab",
	"bemba_10k":                   "68c1b86d",
	"bemba_1k":                    "c35d37f4",
	"bosnian":                     "82a3b50d",
	"bosnian_4k":                  "bf781893",
	"bulgarian":                   "e160608b",
	"bulgarian_1k":                "8a5d100b",
	"bulgarian_latin":             "3fc75aa9",
	"bulgarian_latin_1k":          "53bc1d4f",
	"catalan":                     "e2659e9e",
	"catalan_1k":                  "d386f5d7",
	"chinese":                     "2557d6b5",
	"chinese_10k":                 "426dde18",
	"chinese_1k":                  "87985165",
	"chinese_50k":                 "58325ba1",
	"chinese_5k":                  "fa33d3ce",
	"code_6502_assembly":          "6f5aa5c7",
	"code_abap":                   "beb7e2a7",
	"code_abap_1k":                "71ee98f7",
	"code_arduino":                "339f11f8",
	"code_assembly":               "a8ef4765",
	"code_bash":                   "a27c6aeb",
	"code_c":                      "ed38ae5d",
	"code_clojure":                "84b456fd",
	"code_cobol":                  "73ddcc26",
	"code_cpp":                    "8cfa37fd",
	"code_csharp":                 "e136da59",
	"code_css":                    "55ccd317",
	"code_dart":                   "d2cd17fa",
	"code_elixir":                 "14eefb57",
	"code_erlang":                 "cd89d48b",
	"code_fortran":                "fb9b5dfc",
	"code_fsharp":                 "e1056d72",
	"code_gdscript":               "b56d1585",
	"code_gdscript_2":             "dacf95c7",
	"code_go":                     "c8e5763c",
	"code_haskell":                "384bacc5",
	"code_html":                   "7f2498ed",
	"code_java":                   "a6bb85d0",
	"code_javascript":             "d07533da",
	"code_javascript_1k":          "8df55906",
	"code_javascript_react":       "4b36099b",
	"code_jule":                   "15b3438c",
	"code_julia":                  "afcabb6c",
	"code_kotlin":                 "e4acdcaf",
	"code_lua":                    "5fb074a6",
	"code_luau":                   "64a9baa7",
	"code_matlab":                 "db055f76",
	"code_nim":                    "cf0ea388",
	"code_nix":                    "bb95115c",
	"code_ocaml":                  "a59d3753",
	"code_odin":                   "3f2f7685",
	"code_ook":                    "2f84acd1",
	"code_opencl":                 "72e5d9d3",
	"code_pascal":                 "b09f5a1a",
	"code_perl":                   "0e01903b",
	"code_php":                    "f184028d",
	"code_python":                 "4bbad7a0",
	"code_python_1k":              "80043f02",
	"code_python_2k":              "a27ac735",
	"code_python_5k":              "c27d2911",
	"code_r":                      "49648019",
	"code_r_2k":                   "e3e5727f",
	"code_rockstar":               "e23eb971",
	"code_ruby":                   "442dac19",
	"code_rust":                   "e5d6bf31",
	"code_scala":                  "fdfbf8e5",
	"code_sql":                    "f88df080",
	"code_swift":                  "72b1b832",
	"code_systemverilog":          "cd574e64",
	"code_typescript":             "2c9cf8b5",
	"code_typst":                  "1e164b02",
	"code_v":                      "9336bb9e",
	"code_vhdl":                   "ab9960ab",
	"code_vim":                    "cf2a0c1b",
	"code_vimscript":              "47c158c0",
	"code_visual_basic":           "acfe8139",
	"code_yoptascript":            "cfe342b4",
	"code_zig":                    "5b8eeec9",
	"croatian":                    "ed934891",
	"croatian_1k":                 "58b2667b",
	"czech":                       "b6f42314",
	"czech_10k":                   "569430a8",
	"czech_1k":                    "b6ee0886",
	"danish":                      "4163cb64",
	"danish_10k":                  "8f666728",
	"danish_1k":                   "6fe54519",
	"docker_file":                 "d43ff3e3",
	"dutch":                       "99c126ac",
	"dutch_1k":                    "0b4f413a",
	"english":                     "be99aa1a",
	"english_10k":                 "14433b1b",
	"english_1k":                  "f7db9046",
	"english_25k":                 "0a1aa712",
	"english_5k":                  "868ddd0b",
	"english_commonly_misspelled": "4ba19d53",
	"english_contractions":        "0892471f",
	"english_doubleletter":        "dc314276",
	"english_medical":             "ce08323d",
	"english_old":                 "ac03ca90",
	"english_shakespearean":       "8bcf50e4",
	"esperanto":                   "bc98d1bf",
	"esperanto_10k":               "74df9b6e",
	"esperanto_1k":                "0cb93be7",
	"esperanto_25k":               "36b1e25b",
	"esperanto_36k":               "98ba63d3",
	"esperanto_h_sistemo":         "1358b52f",
	"esperanto_h_sistemo_10k":     "aae27edf",
	"esperanto_h_sistemo_1k":      "34be2679",
	"esperanto_h_sistemo_25k":     "a34ed007",
	"esperanto_h_sistemo_36k":     "d6243131",
	"esperanto_x_sistemo":         "aae56ccf",
	"esperanto_x_sistemo_10k":     "eedd429d",
	"esperanto_x_sistemo_1k":      "8d6fd19d",
	"esperanto_x_sistemo_25k":     "a9201244",
	"esperanto_x_sistemo_36k":     "0ac37663",
	"estonian":                    "f1894f8d",
	"estonian_10k":                "0e9f1191",
	"estonian_1k":                 "56ad5905",
	"estonian_5k":                 "a67706a7",
	"euskera":                     "e2e06b8a",
	"filipino":                    "589e57f4",
	"filipino_1k":                 "a7d17098",
	"finnish":                     "799b04f4",
	"finnish_10k":                 "1bb7a0dc",
	"finnish_1k":                  "72edf5cf",
	"french":                      "3a153572",
	"french_10k":                  "d6b9983e",
	"french_1k":                   "403f1c3e",
	"french_2k":                   "41eb9358",
	"french_bitoduc":              "cac5f34c",
	"frisian":                     "a2fa9090",
	"frisian_1k":                  "1ac5f941",
	"friulian":                    "ec16c819",
	"galician":                    "083af28c",
	"georgian":                    "5d742e00",
	"german":                      "804728e8",
	"german_10k":                  "80fdce29",
	"german_1k":                   "eabfdb0a",
	"git":                         "e6480e58",
	"greek":                       "ef33294f",
	"greek_10k":                   "13ea83fc",
	"greek_1k":                    "1eb5c094",
	"greek_25k":                   "1c14e862",
	"greek_5k":                    "1c24dea2",
	"greek_koine":                 "59000cd7",
	"greeklish":                   "12ee3b3a",
	"greeklish_10k":               "c6bf427b",
	"greeklish_1k":                "1aa01eb4",
	"greeklish_25k":               "bf58b0ef",
	"greeklish_5k":                "dac9a1dd",
	"gujarati":                    "9a3754dc",
	"gujarati_1k":                 "49d0fdae",
	"hausa":                       "3724dbb0",
	"japanese":                    "92ed2422",
	"russian":                     "f5aacfd2",
	"russian_empire":              "be593606",
	"traditional_chinese":         "58c5a72d",
}

func TestPublishedHashesAreImmutable(t *testing.T) {
	_, reg := sharedDicts(t)

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

// The dictionary formerly published as `css_code` is now published as
// `code_css`, and its dict_hash did NOT move: it is still 55ccd317 in
// publishedHashes above, which is the frozen table every other assertion in
// this file is measured against.
//
// This test is the PROOF that the rename left every old run replayable. A run
// stores the dict_hash it was generated from, and replay resolves the word list
// by that hash alone — never by language key, never by file name. So the
// question a key rename raises is exactly one question: does the digest depend
// on anything but the words? It does not, and the two assertions below say so
// directly rather than by implication:
//
//   - the digest of the word list computed in isolation, with no key and no
//     file involved, equals the published 55ccd317; and
//   - the file name and the document's own `name` field are now both
//     `code_css` where they were both `css_code`, and the hash is unchanged.
//
// Every run recorded against 55ccd317 therefore still resolves to the same
// bytes through Registry.Body, which is what "old runs stay replayable" means
// operationally. If this ever fails, the word list was edited — revert it;
// renaming the key can never be the cause.
func TestRenamingADictionaryKeyDoesNotMoveItsHash(t *testing.T) {
	const (
		oldLang = "css_code"
		newLang = "code_css"
		frozen  = "55ccd317"
	)

	core, reg := sharedDicts(t)

	require.Equal(t, frozen, publishedHashes[newLang],
		"the rename moves the KEY of the frozen row; the VALUE is the published hash and must not move")
	_, stillFrozenUnderTheOldKey := publishedHashes[oldLang]
	require.False(t, stillFrozenUnderTheOldKey, "the old key must not survive alongside the new one")

	var got CatalogueEntry
	for _, e := range reg.Catalogue() {
		require.NotEqual(t, oldLang, e.Lang, "the catalogue still publishes the pre-rename key")
		if e.Lang == newLang {
			got = e
		}
	}
	require.Equal(t, newLang, got.Lang, "the renamed dictionary is missing from the catalogue")
	assert.Equal(t, frozen, got.DictHash, "the catalogue's hash for the renamed dictionary moved")

	// Content addressing, asserted directly: the digest is a function of the
	// WORD LIST and of nothing else. This call passes no key, no file name and
	// no document — just the words — and still lands on the frozen value.
	raw, err := dictFS.ReadFile(path.Join(dictsDir, newLang+".json"))
	require.NoError(t, err)
	var doc dictDoc
	require.NoError(t, json.Unmarshal(raw, &doc))

	fromWordsAlone, err := core.DictVersion(doc.Words)
	require.NoError(t, err)
	assert.Equal(t, frozen, fromWordsAlone,
		"the dict_hash is FNV-1a over the word list; a key rename cannot reach it")

	// The key and the file name both changed. The hash is the same object.
	assert.Equal(t, newLang, doc.Name, "the document's own name field still carries the old key")
	body, ok := reg.Body(frozen)
	require.True(t, ok, "runs recorded against %s no longer resolve to a body", frozen)
	assert.Equal(t, raw, body, "%s must still address the same bytes it always has", frozen)
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

// The documented game has to be PLAYABLE — on every language the server
// publishes, not just on the one the benchmarks happen to use.
//
// MaxWordCount is 10 000 words, and a run that long costs one insert per
// grapheme plus one commit per word. Whether that fits under the ingestion caps
// is a property of the DICTIONARY, not of the caps: code_css averages ~10.7
// characters a token where german averages ~6.9, so a full code_css run is 37%
// more events than a full german run. Sizing the caps against the fixture
// language is exactly the mistake this test exists to catch — it is how the
// caps came to refuse a mode the docs promise.
//
// Punctuation is in the matrix for the non-obvious reason: it LENGTHENS TOKENS,
// so it moves the event count as well as the byte count. The two are coupled,
// and headroom given to one without the other is not headroom.
func TestEveryPublishedDictionaryCanPlayAFullLengthRun(t *testing.T) {
	core, reg := sharedDicts(t)

	worstEvents, worstLang := 0, ""
	for _, entry := range reg.Catalogue() {
		body, ok := reg.Body(entry.DictHash)
		require.True(t, ok, entry.Lang)
		for _, punctuation := range []bool{false, true} {
			events := fullLengthRunEvents(t, core, body, entry.DictHash, punctuation)
			assert.LessOrEqualf(t, events, perf.MaxEvents,
				"a full %d-word %s run (punctuation=%v) is %d events, over the %d cap: "+
					"MaxWordCount is unreachable on this dictionary",
				perf.MaxWordCount, entry.Lang, punctuation, events, perf.MaxEvents)
			if events > worstEvents {
				worstEvents, worstLang = events, entry.Lang
			}
		}
	}
	t.Logf("worst full-length run: %s at %d events (cap %d, %.0f%% used)",
		worstLang, worstEvents, perf.MaxEvents,
		float64(worstEvents)/float64(perf.MaxEvents)*100)
}

// fullLengthRunEvents counts the events a flawless MaxWordCount run on this
// dictionary would emit: one insert per grapheme, one commit per word. The word
// list comes from the real generator in goja, because the decoration rules
// (which is what punctuation changes) live there and nowhere else.
func fullLengthRunEvents(t *testing.T, core *Core, dictBody []byte, dictHash string, punctuation bool) int {
	t.Helper()
	ctx := context.Background()

	dict, err := core.parseJSON(ctx, string(dictBody))
	require.NoError(t, err)

	seedCtx, err := core.parseJSON(ctx, fmt.Sprintf(
		`{"seed":12345,"dictVersion":%q,"generation":{"mode":"words","length":%d,`+
			`"punctuation":%v,"numbers":false,"randomCase":false,"reverse":false}}`,
		dictHash, perf.MaxWordCount, punctuation))
	require.NoError(t, err)

	generated, err := core.call(ctx, core.generateWords, dict, seedCtx)
	require.NoError(t, err)
	value, err := core.unwrapResult(ctx, generated)
	require.NoError(t, err)

	raw, err := core.stringifyJSON(ctx, value.(*goja.Object).Get("words"))
	require.NoError(t, err)
	var words []string
	require.NoError(t, json.Unmarshal(raw, &words))

	events := len(words) // one commit per word
	for _, w := range words {
		events += utf8.RuneCountInString(w) // one insert per grapheme
	}
	return events
}
