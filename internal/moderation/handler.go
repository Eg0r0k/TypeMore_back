package moderation

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/typemore/typemore-server/internal/platform/httpx"
)

// ActorFunc resolves the authenticated admin performing a request. Supplied by
// the composition root (an adapter over the auth middleware's context user);
// declared here so moderation never imports auth. The middlewares this
// subtree is mounted behind guarantee it succeeds — a false return is a
// wiring bug and is answered like any other not-found.
type ActorFunc func(r *http.Request) (Actor, bool)

// Service is the admin HTTP surface over the moderation store
// (docs/MODERATION.md, "The admin surface"). Every route is mounted by the
// composition root behind session auth, the Origin (CSRF) check and a
// permission gate — read routes behind bans:read, mutations behind
// bans:write — and the whole subtree answers 404 to anyone without the
// permission, so to the rest of the world it does not exist.
type Service struct {
	store *Store
	actor ActorFunc
	log   *slog.Logger
}

// NewService wires the admin surface.
func NewService(store *Store, actor ActorFunc, log *slog.Logger) *Service {
	return &Service{store: store, actor: actor, log: log}
}

// AdminRoutes returns the /admin/... subtree. The two permission middlewares
// are passed IN (the same pattern as auth middleware everywhere else): the
// composition root builds them from the auth service, so moderation stays
// free of any authorization vocabulary beyond "this route needs read, that
// one needs write".
func (s *Service) AdminRoutes(requireRead, requireWrite func(http.Handler) http.Handler) http.Handler {
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(requireRead)
		r.Get("/bans", s.handleListBans)
		r.Get("/users/{identifier}/bans", s.handleUserBans)
		// Badges read behind the SAME permission as bans: they are the operator
		// half of a decoration, not a second kind of authority, and a role that
		// can read who is banned is not less trusted with who is a beta tester.
		r.Get("/users/{identifier}/badges", s.handleUserBadges)
		r.Get("/badges/{code}/holders", s.handleBadgeHolders)
	})
	r.Group(func(r chi.Router) {
		r.Use(requireWrite)
		r.Post("/bans", s.handleBan)
		r.Delete("/users/{userID}/ban", s.handleUnban)
		r.Post("/users/{identifier}/badges", s.handleGrantBadge)
		r.Delete("/users/{identifier}/badges/{code}", s.handleRevokeBadge)
	})
	return r
}

// banView is one ban row as the ADMIN surface serves it. reason and the
// issuer are visible here on purpose: MODERATION.md's "the reason is
// internal, always" is about the banned player's surface, and this subtree is
// the internal audience that rule preserves the note for.
type banView struct {
	ID          uuid.UUID  `json:"id"`
	UserID      uuid.UUID  `json:"userId"`
	DisplayName string     `json:"displayName,omitempty"`
	Reason      string     `json:"reason"`
	IssuedBy    string     `json:"issuedBy"`
	IssuedAt    time.Time  `json:"issuedAt"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
	RevokedAt   *time.Time `json:"revokedAt,omitempty"`
	Active      bool       `json:"active"`
}

func toBanView(b Ban, now time.Time) banView {
	return banView{
		ID: b.ID, UserID: b.UserID, DisplayName: b.DisplayName,
		Reason: b.Reason, IssuedBy: b.IssuedBy, IssuedAt: b.IssuedAt,
		ExpiresAt: b.ExpiresAt, RevokedAt: b.RevokedAt,
		Active: b.Active(now),
	}
}

// handleListBans serves GET /admin/bans?active=&limit=. active defaults to
// true — the review queue view; ?active=0 is the full history feed.
func (s *Service) handleListBans(w http.ResponseWriter, r *http.Request) {
	onlyActive := r.URL.Query().Get("active") != "0"
	limit := httpx.ParseLimit(r.URL.Query().Get("limit"), 100, 500)

	bans, err := s.store.List(r.Context(), onlyActive, int32(limit))
	if err != nil {
		s.internalError(w, r, "list bans", err)
		return
	}
	now := time.Now()
	views := make([]banView, len(bans))
	for i := range bans {
		views[i] = toBanView(bans[i], now)
	}
	s.writeJSON(w, http.StatusOK, struct {
		Bans []banView `json:"bans"`
	}{Bans: views})
}

// handleUserBans serves GET /admin/users/{identifier}/bans: resolution plus
// the account's full ban history. {identifier} is a uuid, an email or a
// display name — the same resolution order banctl used (uuid, then email,
// then display name: the unambiguous forms first), with the same refusal on
// ambiguity, rendered as a 409 carrying the candidates instead of a printed
// list.
func (s *Service) handleUserBans(w http.ResponseWriter, r *http.Request) {
	user, ok := s.resolve(w, r, chi.URLParam(r, "identifier"))
	if !ok {
		return
	}
	history, err := s.store.History(r.Context(), user.ID)
	if err != nil {
		s.internalError(w, r, "ban history", err)
		return
	}
	restricted, err := s.store.IsRestricted(r.Context(), user.ID)
	if err != nil {
		s.internalError(w, r, "resolve restriction", err)
		return
	}
	now := time.Now()
	views := make([]banView, len(history))
	for i := range history {
		views[i] = toBanView(history[i], now)
	}
	s.writeJSON(w, http.StatusOK, struct {
		User       userView  `json:"user"`
		Restricted bool      `json:"restricted"`
		Bans       []banView `json:"bans"`
	}{User: userView(user), Restricted: restricted, Bans: views})
}

// handleBan serves POST /admin/bans. Amend semantics and a DIFF response,
// exactly the contract banctl printed: an admin who re-submitted by accident
// must see "nothing changed", one extending an expiry must see what moved —
// "ok" would be true in both cases and useful in neither.
func (s *Service) handleBan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		// User is a uuid, an email or a display name.
		User   string `json:"user"`
		Reason string `json:"reason"`
		// Until is a duration ("72h") or an RFC3339 instant; absent means a
		// PERMANENT ban — an explicit omission rather than a magic zero.
		Until string `json:"until"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if req.Reason == "" {
		// A ban with no note is one nobody can review later.
		s.writeError(w, http.StatusBadRequest, "reason_required", "a ban requires a reason")
		return
	}
	expiresAt, err := parseUntil(req.Until, time.Now())
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_until", "until must be a duration (72h) or an RFC3339 instant in the future")
		return
	}
	user, ok := s.resolve(w, r, req.User)
	if !ok {
		return
	}
	actor, ok := s.actor(r)
	if !ok {
		http.NotFound(w, r)
		return
	}

	res, err := s.store.Ban(r.Context(), user.ID, req.Reason, actor, expiresAt)
	if err != nil {
		s.internalError(w, r, "ban", err)
		return
	}
	s.log.Info("admin: ban",
		"actor", actor.ID, "actorName", actor.Name,
		"target", user.ID, "targetName", user.DisplayName,
		"amended", res.Amended, "expiresAt", res.Ban.ExpiresAt)

	now := time.Now()
	resp := struct {
		User     userView `json:"user"`
		Ban      banView  `json:"ban"`
		Amended  bool     `json:"amended"`
		Previous *banView `json:"previous,omitempty"`
	}{
		User: userView(user),
		Ban:  toBanView(res.Ban, now), Amended: res.Amended,
	}
	if res.Previous != nil {
		prev := toBanView(*res.Previous, now)
		resp.Previous = &prev
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// handleUnban serves DELETE /admin/users/{userID}/ban. The target is a uuid
// only — an unban is precise or it is not issued; the resolution endpoint is
// one GET away. Idempotent like banctl's unban: revoking a ban that is not
// there reports revoked=false rather than erroring, because DELETE run twice
// must mean the same thing as DELETE run once.
func (s *Service) handleUnban(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(chi.URLParam(r, "userID"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_user_id", "the unban target is a user uuid")
		return
	}
	actor, ok := s.actor(r)
	if !ok {
		http.NotFound(w, r)
		return
	}

	ban, err := s.store.Unban(r.Context(), userID, actor)
	if errors.Is(err, ErrNotBanned) {
		s.writeJSON(w, http.StatusOK, struct {
			Revoked bool `json:"revoked"`
		}{Revoked: false})
		return
	}
	if err != nil {
		s.internalError(w, r, "unban", err)
		return
	}
	s.log.Info("admin: unban",
		"actor", actor.ID, "actorName", actor.Name, "target", userID)

	view := toBanView(ban, time.Now())
	s.writeJSON(w, http.StatusOK, struct {
		Revoked bool    `json:"revoked"`
		Ban     banView `json:"ban"`
	}{Revoked: true, Ban: view})
}

type userView struct {
	ID          uuid.UUID `json:"id"`
	DisplayName string    `json:"displayName"`
}

// resolve maps an identifier to exactly one account or writes the refusal:
// 404 for no match (indistinguishable from the subtree's own answer to
// outsiders), 409 with candidates for an ambiguous one — never a guess.
func (s *Service) resolve(w http.ResponseWriter, r *http.Request, identifier string) (User, bool) {
	user, err := s.store.ResolveUser(r.Context(), identifier)
	var ambiguous *ErrAmbiguousUser
	switch {
	case errors.As(err, &ambiguous):
		candidates := make([]userView, len(ambiguous.Candidates))
		for i, c := range ambiguous.Candidates {
			candidates[i] = userView(c)
		}
		s.writeJSON(w, http.StatusConflict, struct {
			Error      string     `json:"error"`
			Candidates []userView `json:"candidates"`
		}{Error: "ambiguous_user", Candidates: candidates})
		return User{}, false
	case errors.Is(err, ErrNoSuchUser):
		http.NotFound(w, r)
		return User{}, false
	case err != nil:
		s.internalError(w, r, "resolve user", err)
		return User{}, false
	}
	return user, true
}

// parseUntil turns the wire `until` into an expiry: empty = permanent (nil),
// a Go duration = now+d, otherwise an RFC3339 instant. A moment not in the
// future is an error — it would mint a ban already lapsed.
func parseUntil(until string, now time.Time) (*time.Time, error) {
	if until == "" {
		return nil, nil
	}
	if d, err := time.ParseDuration(until); err == nil {
		if d <= 0 {
			return nil, errors.New("non-positive duration")
		}
		t := now.Add(d)
		return &t, nil
	}
	t, err := time.Parse(time.RFC3339, until)
	if err != nil {
		return nil, err
	}
	if !t.After(now) {
		return nil, errors.New("instant in the past")
	}
	return &t, nil
}

func (s *Service) writeJSON(w http.ResponseWriter, status int, v any) {
	if err := httpx.WriteJSON(w, status, v); err != nil {
		s.log.Error("moderation: encode response", "err", err)
	}
}

func (s *Service) writeError(w http.ResponseWriter, status int, code, message string) {
	s.writeJSON(w, status, struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}{Error: code, Message: message})
}

func (s *Service) internalError(w http.ResponseWriter, r *http.Request, op string, err error) {
	s.log.Error("moderation: "+op, "err", err, "path", r.URL.Path)
	s.writeError(w, http.StatusInternalServerError, "internal", "internal error")
}
