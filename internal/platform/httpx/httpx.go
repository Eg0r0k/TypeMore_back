// Package httpx holds the small HTTP-boundary helpers that every domain
// handler package otherwise grows its own copy of: query-limit clamping, the
// rate-limit client key, the If-None-Match comparison and the opaque
// keyset-cursor token plumbing. It is deliberately platform-level — nothing
// here knows any domain, and per the platform layering rule nothing here may
// come to.
package httpx

import (
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// ParseLimit clamps a ?limit= query value to [1, max], returning def on absent
// or unparseable input.
func ParseLimit(raw string, def, max int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// ClientIP is the rate-limit key for unauthenticated endpoints. RemoteAddr
// only: the server is expected to sit behind a proxy that sets it, and
// trusting a forwarded header would make the limit trivially bypassable.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ETagMatches implements the If-None-Match comparison for strong ETags: "*"
// matches anything, otherwise any list member equal to the tag (weak prefixes
// are tolerated — the weak comparison function is the correct one for
// If-None-Match, RFC 9110 §13.1.2).
func ETagMatches(ifNoneMatch, etag string) bool {
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

// EncodeCursor packs keyset-cursor fields into an opaque base64url token. The
// fields are joined with ':', so no field except the last may ever contain
// one — each caller's field set is chosen to guarantee that.
func EncodeCursor(fields ...string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(fields, ":")))
}

// DecodeCursor reverses EncodeCursor, rejecting a token that does not decode
// or does not split into exactly n fields. Parsing and range-checking the
// fields stays with the caller: the token is client input like any other.
func DecodeCursor(token string, n int) ([]string, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(string(b), ":")
	if len(parts) != n {
		return nil, errors.New("httpx: malformed cursor")
	}
	return parts, nil
}
