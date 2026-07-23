package auth

import (
	"context"
	"net/http"
)

// ctxKey is a private type for context keys so no other package can collide.
type ctxKey int

const userCtxKey ctxKey = iota

// UserFrom returns the authenticated user attached by RequireAuth, if any.
func UserFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(userCtxKey).(User)
	return u, ok
}

// withUser returns a copy of ctx carrying u.
func withUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, userCtxKey, u)
}

// RequireAuth is middleware that rejects unauthenticated requests with 401 and
// otherwise attaches the resolved User to the request context (read it with
// UserFrom). Handlers behind it can assume a user is present.
func (s *Service) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := s.authenticate(r.Context(), r)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(withUser(r.Context(), user)))
	})
}

// RequireOrigin is middleware enforcing that state-changing requests carry an
// Origin header matching the configured frontend origin. This is the CSRF
// defense for cookie-authenticated endpoints: a cross-site page can trigger a
// request but cannot forge the Origin header. Safe methods (GET/HEAD/OPTIONS)
// are exempt.
//
// Note: OAuth callbacks are top-level GET navigations from the provider and are
// therefore not subject to this check; they are protected by the state cookie
// instead.
func (s *Service) RequireOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Origin") != s.cfg.FrontendOrigin {
			s.writeError(w, r, apiErrForbiddenOrigin)
			return
		}
		next.ServeHTTP(w, r)
	})
}
