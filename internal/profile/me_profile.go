package profile

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/typemore/typemore-server/internal/badges"
)

// PATCH /api/v1/me/profile — the owner-editable half of a profile: the bio, the
// board they type on, where else to find them, and which badges they show.
//
// ONE ROUTE FOR ALL FOUR because they are one gesture: the settings screen has
// a single save button, and four routes would make a half-saved screen a state
// the product has to explain. The store writes them in one transaction for the
// same reason.
//
// Deliberately NOT on /me/settings, which owns the two PRIVACY switches. Those
// decide who may read the profile; these are the profile. Mixing them would
// make "I edited my bio" and "I opened my profile to strangers" the same
// request, and a client retrying one would resend the other.

// Length ceilings, in RUNES. The same numbers as the schema's CHECKs (00029),
// stated here too because this copy is what produces a decent refusal and that
// one is what stays true for rows arriving by a path this handler is not on.
// Runes rather than bytes on both sides: a byte cap silently gives a Cyrillic
// bio half the room an ASCII one gets.
const (
	bioMaxLen      = 250
	keyboardMaxLen = 100
	// A showcase longer than this is not a showcase. The cap is on the REQUEST
	// rather than on what can be granted: it bounds the work this handler does
	// before it has looked anything up.
	showcaseMaxLen = 16
)

// profilePatchRequest is the wire shape. Every field is a pointer or a map so
// "not mentioned" and "set to empty" stay different instructions — the whole
// point of a PATCH.
type profilePatchRequest struct {
	// Bio/Keyboard: `""` clears, absent leaves alone.
	Bio      *string `json:"bio"`
	Keyboard *string `json:"keyboard"`
	// Links maps a kind to a HANDLE (never a URL — links.go). `""` removes it.
	Links map[string]string `json:"links"`
	// Showcase is the badge codes to display, in order. `[]` shows none;
	// absent leaves the arrangement alone.
	Showcase *[]string `json:"showcase"`
}

// HandleUpdateProfile serves PATCH /api/v1/me/profile. Mounted behind
// RequireOrigin + RequireAuth by the composition root; answers with the same
// owner view the settings screen re-renders from.
func (s *Service) HandleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.userID(r.Context())
	if !ok {
		s.writeError(w, r, apiErrUnauthorized)
		return
	}

	var req profilePatchRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxProfileBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, apiErrBadRequest("request body is not valid JSON"))
		return
	}
	if req.Bio == nil && req.Keyboard == nil && req.Links == nil && req.Showcase == nil {
		// An empty patch is a client bug, not a no-op write — the same answer
		// /me/settings gives to the same mistake.
		s.writeError(w, r, apiErrBadRequest("no profile fields to update"))
		return
	}

	patch := Patch{}

	if req.Bio != nil {
		value, err := s.normalizeText(*req.Bio, bioMaxLen, "bio")
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		patch.Bio, patch.SetBio = value, true
	}
	if req.Keyboard != nil {
		value, err := s.normalizeText(*req.Keyboard, keyboardMaxLen, "keyboard")
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		patch.Keyboard, patch.SetKeyboard = value, true
	}

	if req.Links != nil {
		patch.Links = make(map[string]string, len(req.Links))
		for kind, handle := range req.Links {
			handle = strings.TrimSpace(handle)
			if handle == "" {
				// Clearing. The kind is still checked: a request naming a
				// service this build does not link to is a client that has
				// drifted from the contract, and answering "fine" to it would
				// hide that until somebody wonders why nothing saved.
				if !s.knownLinkKind(kind) {
					s.writeError(w, r, apiErrBadRequest("unknown link kind: "+kind))
					return
				}
				patch.Links[kind] = ""
				continue
			}
			if err := ValidateLink(kind, handle); err != nil {
				s.writeError(w, r, apiErrBadRequest(err.Error()))
				return
			}
			patch.Links[kind] = handle
		}
	}

	if req.Showcase != nil {
		codes := *req.Showcase
		if len(codes) > showcaseMaxLen {
			s.writeError(w, r, apiErrBadRequest("too many badges in the showcase"))
			return
		}
		// Validated against what this account actually HOLDS, not against the
		// known-code list: a real code somebody else was granted is exactly the
		// request this check exists to refuse.
		granted, err := s.store.GrantedBadges(r.Context(), userID)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		held := make(map[string]struct{}, len(granted))
		for _, g := range granted {
			held[g.Code] = struct{}{}
		}
		seen := make(map[string]struct{}, len(codes))
		for _, code := range codes {
			if _, ok := held[code]; !ok {
				// One answer for "you were never granted this" and "no such
				// badge exists": a refusal that distinguishes them is a probe
				// for which badges other people have.
				s.writeError(w, r, apiErrBadRequest("that badge is not yours to show"))
				return
			}
			if _, dup := seen[code]; dup {
				s.writeError(w, r, apiErrBadRequest("a badge cannot appear twice in the showcase"))
				return
			}
			seen[code] = struct{}{}
		}
		// Non-nil even when empty: "show none" is an instruction, and nil is
		// what means "the request was not about badges".
		patch.Showcase = codes
	}

	if err := s.store.ApplyProfilePatch(r.Context(), userID, patch); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.serveOwnProfileIdentity(w, r, userID)
}

// maxProfileBodyBytes bounds the patch. Generous next to the fields it carries
// (250 + 100 characters, three handles, sixteen codes) and tiny next to what an
// unbounded decoder would accept.
const maxProfileBodyBytes = 16 << 10

// normalizeText trims a free-text field and maps the empty result onto NULL.
//
// The empty string is never STORED: the schema forbids it, so that "" can mean
// exactly one thing on the wire — clear this — instead of being a second way to
// spell "unset" that clients would have to handle on read.
func (s *Service) normalizeText(value string, maxLen int, field string) (*string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(value) > maxLen {
		return nil, apiErrBadRequest(field + " is too long")
	}
	// Control characters are stripped rather than refused: they arrive from
	// paste, not from intent, and refusing a paste for an invisible character
	// is a refusal the user cannot act on. Newlines survive — a bio is prose.
	cleaned := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	if cleaned = strings.TrimSpace(cleaned); cleaned == "" {
		return nil, nil
	}
	return &cleaned, nil
}

// knownLinkKind reports whether kind is a service this build links to.
func (s *Service) knownLinkKind(kind string) bool {
	_, ok := linkPatterns[kind]
	return ok
}

// ownIdentityView is what the settings screen re-renders from after a save: the
// stored profile PLUS the pool of badges to arrange (every live grant, shown or
// not). The public surface deliberately serves less — only the showcase.
type ownIdentityView struct {
	Bio      *string    `json:"bio"`
	Keyboard *string    `json:"keyboard"`
	Links    []linkView `json:"links"`
	// Badges is the whole pool; `shown` is what puts one in the showcase and
	// `order` is where. A held-but-hidden badge is how a user un-shows one
	// without it being taken away, so both states have to be visible here.
	Badges []ownBadgeView `json:"badges"`
	// The codes this build knows, so a client can render a grant of a code its
	// own registry has not heard of as an honest unknown rather than as blank.
	KnownBadges []string `json:"knownBadges"`
}

type linkView struct {
	Kind   string `json:"kind"`
	Handle string `json:"handle"`
}

type ownBadgeView struct {
	Code  string `json:"code"`
	Shown bool   `json:"shown"`
	Order *int32 `json:"order,omitempty"`
}

// serveOwnProfileIdentity answers the owner's own view of their profile.
func (s *Service) serveOwnProfileIdentity(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
	identity, err := s.store.Identity(r.Context(), userID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	links, err := s.store.Links(r.Context(), userID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	granted, err := s.store.GrantedBadges(r.Context(), userID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	view := ownIdentityView{
		Bio:         identity.Bio,
		Keyboard:    identity.Keyboard,
		Links:       make([]linkView, len(links)),
		Badges:      make([]ownBadgeView, len(granted)),
		KnownBadges: badges.Codes(),
	}
	for i, l := range links {
		view.Links[i] = linkView(l)
	}
	for i, b := range granted {
		view.Badges[i] = ownBadgeView{Code: b.Code, Shown: b.Order != nil, Order: b.Order}
	}
	s.writeJSON(w, http.StatusOK, view)
}

// HandleOwnProfile serves GET /api/v1/me/profile — the same view the PATCH
// answers with, so the settings screen loads and saves through one shape.
func (s *Service) HandleOwnProfile(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.userID(r.Context())
	if !ok {
		s.writeError(w, r, apiErrUnauthorized)
		return
	}
	s.serveOwnProfileIdentity(w, r, userID)
}
