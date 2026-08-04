package profile

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
)

// Social links: which services a profile may name, and what a handle for each
// one is allowed to look like.
//
// THIS FILE IS THE SOURCE OF TRUTH for the shapes. The frontend duplicates them
// (for a red border before a round trip) and the database duplicates them again
// as CHECK constraints (00029), which is three copies of one rule — deliberate,
// and ordered by what each is for: the client's copy is a courtesy, this one
// produces the refusal a client is answered with, and the database's is what
// stays true for rows arriving by a path neither is on.
//
// WE STORE A HANDLE AND NEVER A URL. A stored URL is a stored redirect: it can
// name any host, including one wearing a lookalike domain, and `javascript:`
// is a URL too. The renderer pastes the handle onto a prefix from a fixed list
// it owns (the frontend's `LINK_PREFIXES`), so the set of hosts this product
// can link to is a list in the source rather than a column in the database.
// Every pattern below therefore excludes `:`, `/`, `.`-runs and whitespace by
// construction — which is what makes "somebody pasted the whole URL in" a
// refusal rather than a broken link on a public page.

// ErrUnknownLinkKind is returned for a service this build does not link to.
var ErrUnknownLinkKind = errors.New("profile: unknown link kind")

// linkPatterns is the vocabulary: kind → the handle grammar it accepts.
//
// Anchored, and bounded on both ends. Each is the platform's own rule reduced
// to what a regex can state honestly — where a rule cannot be expressed (see
// GitHub's double hyphen), the pattern is deliberately the LOOSER one and the
// consequence is a link that 404s, never a link that goes somewhere else.
var linkPatterns = map[string]*regexp.Regexp{
	// GitHub: alphanumerics and single hyphens, 1–39 characters, never leading
	// or trailing a hyphen.
	"github": regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`),
	// YouTube handles, the @name form — stored WITHOUT the @, so the renderer
	// owns the sigil exactly as it owns the host.
	"youtube": regexp.MustCompile(`^[A-Za-z0-9._-]{3,30}$`),
	// Twitch logins: letters, digits and underscores, 4–25 characters.
	"twitch": regexp.MustCompile(`^[A-Za-z0-9_]{4,25}$`),
}

// LinkKinds lists the services a profile may name, sorted.
func LinkKinds() []string {
	out := make([]string, 0, len(linkPatterns))
	for kind := range linkPatterns {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

// ValidateLink reports whether handle is a legal handle for kind.
//
// An EMPTY handle is not validated here and never reaches this function: the
// PATCH treats it as "remove this link" (docs/PROFILE.md), because storing an
// empty handle would make "has no GitHub" and "has a broken GitHub" the same
// row. Callers delete instead.
func ValidateLink(kind, handle string) error {
	pattern, ok := linkPatterns[kind]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownLinkKind, kind)
	}
	if !pattern.MatchString(handle) {
		// The message names the SERVICE and not the pattern: a user pasting a
		// URL needs to be told this field wants a handle, not to be shown a
		// regex they then have to satisfy by guesswork.
		return fmt.Errorf("that is not a valid %s handle — enter the name only, not a link", kind)
	}
	return nil
}
