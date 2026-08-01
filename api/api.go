// Package api ships the machine-readable API contract. openapi.yaml is
// maintained BY HAND against the handler view structs (the json tags are the
// wire truth) — any change to a request or response shape updates the spec in
// the same commit, and review is the enforcement, the same way the layering
// rules are enforced. The spec is embedded and served by the running server so
// the frontend has one URL to point Swagger UI / Redoc / openapi-typescript
// at, and so the document a client reads always describes the binary that is
// answering it — a spec fetched from the instance cannot be a version behind
// the instance.
package api

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"

	"github.com/typemore/typemore-server/internal/platform/httpx"
)

//go:embed openapi.yaml
var spec []byte

// specETag is content-derived, so a re-deploy with an unchanged spec keeps the
// clients' caches warm and a changed one busts them — the same discipline the
// dictionary catalogue uses.
var specETag = `"` + hex.EncodeToString(func() []byte { h := sha256.Sum256(spec); return h[:8] }()) + `"`

// Handler serves the embedded OpenAPI document.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("ETag", specETag)
		if httpx.ETagMatches(r.Header.Get("If-None-Match"), specETag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(spec)
	}
}
