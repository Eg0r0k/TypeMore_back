package middleware

import "net/http"

func CORSMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // Set CORS headers for the preflight request
        if r.Method == http.MethodOptions {
            w.Header().Set("Access-Control-Allow-Origin", "http://localhost:5173") // Allow requests from this origin
            w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS") // Allowed methods
            w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type") // Allowed headers
            w.WriteHeader(http.StatusNoContent)
            return
        }

        w.Header().Set("Access-Control-Allow-Origin", "http://localhost:5173")


        next.ServeHTTP(w, r)
    })
}