package replay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Cache-Control policies.
const (
	// bodyCacheControl: a dictionary body is addressed BY the hash of its
	// content, so the bytes behind a URL can never change. Publishing a new
	// version of a language mints a new hash — a new URL — so caching forever
	// is not a bet, it is a property of the addressing scheme.
	bodyCacheControl = "public, max-age=31536000, immutable"
	// catalogueCacheControl: the catalogue is NOT immutable — adding a language
	// or republishing one changes it. Short freshness plus an ETag keeps the
	// revalidation cheap (304, no body) without letting clients pin a stale
	// language list.
	catalogueCacheControl = "public, max-age=60"
)

// DictionaryService serves the seeded registry over HTTP. Everything it writes
// was built at startup; a request only picks an encoding and copies bytes.
type DictionaryService struct {
	reg       *Registry
	catalogue asset
}

// asset is a response body in both encodings plus its validator.
type asset struct {
	raw  []byte
	gz   []byte
	etag string
}

// NewDictionaryService pre-renders the catalogue so the JSON is marshalled and
// compressed exactly once.
func NewDictionaryService(reg *Registry) (*DictionaryService, error) {
	body, err := json.Marshal(reg.Catalogue())
	if err != nil {
		return nil, fmt.Errorf("replay: marshal dictionary catalogue: %w", err)
	}
	gz, err := gzipBytes(body)
	if err != nil {
		return nil, fmt.Errorf("replay: compress dictionary catalogue: %w", err)
	}
	// The catalogue has no natural content hash (dict_hash fingerprints one word
	// list, not the list of lists), so its validator is a plain digest of the
	// rendered bytes.
	sum := sha256.Sum256(body)
	return &DictionaryService{
		reg:       reg,
		catalogue: asset{raw: body, gz: gz, etag: `"` + hex.EncodeToString(sum[:8]) + `"`},
	}, nil
}

// Routes returns the catalogue router, mounted at /api/v1/dictionaries.
// Dictionaries are public assets: no session, no Origin check.
func (s *DictionaryService) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", s.handleCatalogue)
	return r
}

// StaticRoutes returns the body router, mounted at /static/dictionaries.
func (s *DictionaryService) StaticRoutes() http.Handler {
	r := chi.NewRouter()
	r.Get("/{dictHash}.json", s.handleBody)
	return r
}

// handleCatalogue answers with [{ lang, name, dictHash, wordCount, bytes }] —
// how a client discovers which languages exist and which URL holds each body.
func (s *DictionaryService) handleCatalogue(w http.ResponseWriter, r *http.Request) {
	writeAsset(w, r, s.catalogue, catalogueCacheControl)
}

// handleBody answers with the dictionary whose word list hashes to {dictHash}.
// An unknown hash is a 404, never a redirect to "the current" version: a run
// recorded against a retired dictionary must fail loudly, not silently replay
// against different words.
func (s *DictionaryService) handleBody(w http.ResponseWriter, r *http.Request) {
	e, ok := s.reg.byHash[chi.URLParam(r, "dictHash")]
	if !ok {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found","message":"no dictionary with that hash"}` + "\n"))
		return
	}
	writeAsset(w, r, asset{raw: e.raw, gz: e.gz, etag: `"` + e.DictHash + `"`}, bodyCacheControl)
}

// writeAsset serves a pre-built body: conditional request first, then the
// encoding the client accepts. Nothing is allocated on this path.
func writeAsset(w http.ResponseWriter, r *http.Request, a asset, cacheControl string) {
	h := w.Header()
	h.Set("Cache-Control", cacheControl)
	h.Set("ETag", a.etag)
	// The response varies by encoding, and a shared cache MUST NOT hand a
	// gzipped body to a client that did not ask for one.
	h.Add("Vary", "Accept-Encoding")

	if etagMatches(r.Header.Get("If-None-Match"), a.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	body := a.raw
	if acceptsGzip(r.Header.Get("Accept-Encoding")) {
		body = a.gz
		h.Set("Content-Encoding", "gzip")
	}
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

// etagMatches implements the If-None-Match comparison for our strong ETags:
// "*" matches anything, otherwise any list member equal to the tag (weak
// prefixes are tolerated — the weak comparison function is the correct one for
// If-None-Match).
func etagMatches(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" {
		return false
	}
	for candidate := range strings.SplitSeq(ifNoneMatch, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

// acceptsGzip reports whether the client accepts gzip, honouring an explicit
// "gzip;q=0" refusal.
func acceptsGzip(acceptEncoding string) bool {
	for part := range strings.SplitSeq(acceptEncoding, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		for param := range strings.SplitSeq(params, ";") {
			key, value, found := strings.Cut(strings.TrimSpace(param), "=")
			if found && strings.EqualFold(key, "q") && isZeroQuality(value) {
				return false
			}
		}
		return true
	}
	return false
}

// isZeroQuality reports whether a q-value denies the encoding ("0", "0.0", …).
func isZeroQuality(v string) bool {
	q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	return err == nil && q == 0
}
