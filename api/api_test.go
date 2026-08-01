package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The spec endpoint's whole contract: the embedded document comes back as
// YAML, and the content-derived ETag turns a re-fetch into a 304.
func TestSpecIsServedWithConditionalCaching(t *testing.T) {
	h := Handler()

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.yaml", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "yaml") {
		t.Fatalf("Content-Type = %q, want yaml", ct)
	}
	if !strings.Contains(rec.Body.String(), "openapi: 3.0") {
		t.Fatal("body does not look like an OpenAPI 3 document")
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/openapi.yaml", http.NoBody)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("conditional GET = %d, want 304", rec.Code)
	}
}
