package auth

import "net/http"

// Permission is one named capability of the admin surface — the vocabulary
// enforcement speaks. Handlers and routes ask for a Permission, never for a
// role: the role is a stored fact (users.role, 00023) that the map below
// expands, so a future 'moderator' tier is a new map entry granting a subset,
// not a sweep over every role check in the tree.
type Permission string

const (
	// PermBansRead covers the read half of moderation: the ban list, an
	// account's ban history, and the identifier resolution that fronts them.
	PermBansRead Permission = "bans:read"
	// PermBansWrite covers issuing, amending and revoking bans.
	PermBansWrite Permission = "bans:write"
	// PermReportsRead covers the report queue and one subject's report history
	// (docs/REPORTS.md).
	PermReportsRead Permission = "reports:read"
	// PermReportsWrite covers resolving a subject's open reports. It is
	// deliberately NOT the permission that bans or withdraws: resolving records
	// a verdict, and the act it refers to is gated by its own permission, so a
	// role could one day triage the queue without being able to punish.
	PermReportsWrite Permission = "reports:write"
	// PermQuotesWrite covers withdrawing a quote from circulation and putting
	// it back. It does NOT cover publishing or editing one — no permission
	// does, because the importer is the only writer of a quote's bytes.
	PermQuotesWrite Permission = "quotes:write"
	// PermRunsReviewRead covers the review queue and one run's override
	// history: reading which runs the policy is unsure about, and who has
	// already decided anything about them.
	PermRunsReviewRead Permission = "runs:review"
	// PermRunsOverride covers overruling the replay worker's verdict on a run.
	//
	// Deliberately its own permission and NOT part of the read one. The worker's
	// verdict is the system's only automatic answer to "is this real", and a
	// run's status decides whether it holds a leaderboard slot, so overruling it
	// is the single most consequential thing the admin surface can do — a role
	// that triages the queue should be able to do so without also being able to
	// put a result back on the board.
	PermRunsOverride Permission = "runs:override"
)

// rolePermissions is the whole authorization model, in one place, versioned
// with the binary that enforces it — deliberately code, not a database table
// (00023's header records the trade). 'player' is absent: no permissions is
// the zero value, not an entry.
var rolePermissions = map[string][]Permission{
	"admin": {
		PermBansRead, PermBansWrite,
		PermReportsRead, PermReportsWrite,
		PermQuotesWrite,
		PermRunsReviewRead, PermRunsOverride,
	},
}

// Can reports whether the user's role grants p.
func (u User) Can(p Permission) bool {
	for _, granted := range rolePermissions[u.Role] {
		if granted == p {
			return true
		}
	}
	return false
}

// Permissions returns the user's expanded permission set as wire strings —
// what /me serves so the client knows which surfaces to render. Empty (nil)
// for a plain player, which the view omits entirely.
func (u User) Permissions() []string {
	granted := rolePermissions[u.Role]
	if len(granted) == 0 {
		return nil
	}
	out := make([]string, len(granted))
	for i, p := range granted {
		out[i] = string(p)
	}
	return out
}

// RequirePermission is middleware gating a subtree on one permission. It must
// be chained AFTER RequireAuth (it reads the user RequireAuth attached).
//
// The refusal is a plain 404, byte-identical to the router's own unknown-route
// answer — deliberately not a 403. To anyone without the permission the admin
// subtree does not exist: a 403 would confirm to every authenticated user that
// there is something there to want, which is the same reason the public replay
// surface answers every failure with one indistinguishable 404
// (docs/MODERATION.md, "The admin surface").
func (s *Service) RequirePermission(p Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, ok := UserFrom(r.Context())
			if !ok || !user.Can(p) {
				http.NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
