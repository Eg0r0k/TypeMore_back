package middleware

import (
	"context"
	"net/http"
	"typeMore/internal/services/jwt"

	"github.com/gorilla/mux"
)

func TokenValidationMiddleware(tokenService *jwt.TokenService) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			tokenStr := authHeader[len("Bearer "):]
			claims, err := tokenService.ValidateAccessToken(tokenStr)
			if err != nil {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), "userClaims", claims)
			r = r.WithContext(ctx)

		
			next.ServeHTTP(w, r)
		})
	}
}
