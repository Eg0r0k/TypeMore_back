// Package badges owns the list of badge CODES this server knows, and nothing
// else about a badge.
//
// WHY A LEAF PACKAGE OF ONE LIST. Two domains need it and domains do not import
// each other: moderation must refuse to GRANT a code it does not know, and
// profile must refuse to SHOWCASE one. A value both need lives below both —
// the same placement runlimits and runstatus have, and for the same reason. Put
// in either domain it would become an import the layering forbids, or a second
// copy of the list kept in step by nobody.
package badges

import (
	"errors"
	"sort"
)

// The badge CODES this server knows, and nothing else about a badge.
//
// WHAT IS DELIBERATELY ABSENT. No name, no description, no icon, no colour, no
// ordering. Those live in the frontend registry
// (`src/entities/badge/registry.ts`) because they are presentation: they are
// rendered by one client and never queried, compared or sorted by the server.
// The same reasoning as the role→permission map (internal/auth/permissions.go),
// pointed the other way — that map is code because ENFORCEMENT reads it, this
// list is code because enforcement reads only the code itself.
//
// What the server does with this list is one thing: refuse to grant a code it
// does not know. That is the whole contract. It is what keeps a typo'd grant
// from becoming a row that renders as a blank chip on a public page, and it is
// why the list has to exist on this side at all rather than trusting an admin
// client to send only real codes.
//
// ADDING ONE is a commit here plus a commit in the registry, in that order and
// ideally in the same change. REMOVING one does not invalidate history: a grant
// of a retired code still reads back (the store never re-validates), it simply
// cannot be granted again, and the client renders what it can. That asymmetry
// is intentional — a badge is a thing that happened, and the record of it must
// not depend on the product still offering it.
var knownBadges = map[string]struct{}{
	// Operators and the people who keep the thing running.
	"staff":       {},
	"contributor": {},
	// Awarded for what someone did, by hand, by a moderator. There is no
	// automatic granting anywhere in this server — see docs/PROFILE.md.
	"tournament_winner": {},
	"beta_tester":       {},
	"bug_hunter":        {},
	"translator":        {},
}

// ErrUnknownBadge is returned when a grant names a code this build does not
// know. Deliberately a refusal and not a silent insert: the database's CHECK
// only bounds the code's SHAPE (00029), so this is the only thing standing
// between a mistyped grant and a row nobody can render.
var ErrUnknown = errors.New("badges: unknown badge code")

// Known reports whether code is a badge this build can grant.
func Known(code string) bool {
	_, ok := knownBadges[code]
	return ok
}

// Codes lists every known code, sorted — the admin surface's vocabulary, so an
// operator picks from a list rather than typing a code from memory.
func Codes() []string {
	out := make([]string, 0, len(knownBadges))
	for code := range knownBadges {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}
