package moderation

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/typemore/typemore-server/internal/badges"
	"github.com/typemore/typemore-server/internal/platform/httpx"
)

// The badge half of the admin surface. Mounted by AdminRoutes beside the bans,
// behind the same permission gates: reading a grant is a read, giving one is a
// write. It gets no permission of its own — a badge is a decoration, and a role
// that can ban somebody is not meaningfully more trusted by also being allowed
// to hand out a chip that says "beta tester".

// badgeGrantView is one grant on the admin surface.
type badgeGrantView struct {
	Code      string    `json:"code"`
	GrantedAt time.Time `json:"grantedAt"`
	// Empty when the granting account is gone (SET NULL — the decision outlives
	// the admin who made it) or when nothing with an account behind it granted.
	GrantedBy string     `json:"grantedBy,omitempty"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
	RevokedBy string     `json:"revokedBy,omitempty"`
	// Granted is the live flag, spelled out so a client renders history without
	// re-deriving the predicate from revokedAt.
	Granted bool `json:"granted"`
	// Shown is whether the badge's OWNER put it in their showcase. An operator
	// seeing "granted but not shown" is seeing a normal state, not a bug.
	Shown bool `json:"shown"`
}

func toBadgeGrantView(g BadgeGrant) badgeGrantView {
	view := badgeGrantView{
		Code: g.Code, GrantedAt: g.GrantedAt, RevokedAt: g.RevokedAt,
		Granted: g.Granted(), Shown: g.Order != nil,
	}
	if g.GrantedByName != nil {
		view.GrantedBy = *g.GrantedByName
	}
	if g.RevokedByName != nil {
		view.RevokedBy = *g.RevokedByName
	}
	return view
}

// handleUserBadges serves GET /admin/users/{identifier}/badges — one account's
// grants, live and revoked.
//
// It is what makes revocation possible at all: an operator cannot take away a
// badge they cannot see, and the codes are not something anyone is expected to
// remember per account. `knownBadges` rides along so the admin UI can offer the
// grant vocabulary from the same response rather than hardcoding a second copy.
func (s *Service) handleUserBadges(w http.ResponseWriter, r *http.Request) {
	user, ok := s.resolve(w, r, chi.URLParam(r, "identifier"))
	if !ok {
		return
	}
	grants, err := s.store.BadgesOfUser(r.Context(), user.ID)
	if err != nil {
		s.internalError(w, r, "list user badges", err)
		return
	}
	views := make([]badgeGrantView, len(grants))
	for i := range grants {
		views[i] = toBadgeGrantView(grants[i])
	}
	s.writeJSON(w, http.StatusOK, struct {
		User        userView         `json:"user"`
		Badges      []badgeGrantView `json:"badges"`
		KnownBadges []string         `json:"knownBadges"`
	}{User: userView(user), Badges: views, KnownBadges: badges.Codes()})
}

// handleBadgeHolders serves GET /admin/badges/{code}/holders — who currently
// holds one badge. Read-only and cheap (one partial index, 00029); it is what
// answers "did the tournament grants all land" without walking accounts.
func (s *Service) handleBadgeHolders(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	limit := httpx.ParseLimit(r.URL.Query().Get("limit"), 100, 500)
	holders, err := s.store.HoldersOfBadge(r.Context(), code, int32(limit))
	if err != nil {
		s.internalError(w, r, "list badge holders", err)
		return
	}
	type holderView struct {
		UserID      uuid.UUID `json:"userId"`
		DisplayName string    `json:"displayName"`
		GrantedAt   time.Time `json:"grantedAt"`
	}
	views := make([]holderView, len(holders))
	for i, h := range holders {
		views[i] = holderView(h)
	}
	s.writeJSON(w, http.StatusOK, struct {
		Code    string       `json:"code"`
		Holders []holderView `json:"holders"`
	}{Code: code, Holders: views})
}

// handleGrantBadge serves POST /admin/users/{identifier}/badges {"code": …}.
//
// Idempotent, and it answers with the grant either way rather than with an
// error on the second call: an operator who clicked twice needs to see that the
// account has the badge, not to work out whether their first click landed.
func (s *Service) handleGrantBadge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	user, ok := s.resolve(w, r, chi.URLParam(r, "identifier"))
	if !ok {
		return
	}
	actor, ok := s.actor(r)
	if !ok {
		http.NotFound(w, r)
		return
	}

	grant, err := s.store.GrantBadge(r.Context(), user.ID, req.Code, actor.auditID())
	if errors.Is(err, ErrUnknownBadge) {
		// Named, unlike the showcase's deliberately vague refusal: this caller
		// already holds the permission to grant, so telling them the code is
		// not one of ours discloses nothing and saves a support round trip.
		s.writeError(w, http.StatusBadRequest, "unknown_badge", "no such badge code")
		return
	}
	if err != nil {
		s.internalError(w, r, "grant badge", err)
		return
	}
	s.log.Info("admin: badge granted",
		"actor", actor.ID, "actorName", actor.Name,
		"target", user.ID, "targetName", user.DisplayName, "badge", grant.Code)

	s.writeJSON(w, http.StatusOK, struct {
		User  userView       `json:"user"`
		Badge badgeGrantView `json:"badge"`
	}{User: userView(user), Badge: toBadgeGrantView(grant)})
}

// handleRevokeBadge serves DELETE /admin/users/{identifier}/badges/{code}.
//
// Answers `{"revoked": false}` when there was nothing live to revoke, exactly
// as unbanning an unbanned account does: DELETE run twice must mean the same
// thing as DELETE run once.
func (s *Service) handleRevokeBadge(w http.ResponseWriter, r *http.Request) {
	user, ok := s.resolve(w, r, chi.URLParam(r, "identifier"))
	if !ok {
		return
	}
	actor, ok := s.actor(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	code := chi.URLParam(r, "code")

	revoked, err := s.store.RevokeBadge(r.Context(), user.ID, code, actor.auditID())
	if err != nil {
		s.internalError(w, r, "revoke badge", err)
		return
	}
	if revoked {
		s.log.Info("admin: badge revoked",
			"actor", actor.ID, "actorName", actor.Name,
			"target", user.ID, "targetName", user.DisplayName, "badge", code)
	}
	s.writeJSON(w, http.StatusOK, struct {
		User    userView `json:"user"`
		Code    string   `json:"code"`
		Revoked bool     `json:"revoked"`
	}{User: userView(user), Code: code, Revoked: revoked})
}
