package routes

import (
	"net/http"
	"typeMore/api/handlers"
	"typeMore/internal/services"
	"typeMore/internal/services/jwt"

	"github.com/gorilla/mux"
	"go.uber.org/zap"
)

func SetupAuthRoutes(router *mux.Router, userService *services.UserService, tokenService *jwt.TokenService, logger *zap.Logger) {
	userHandler := handlers.NewUserHandler(userService, tokenService, logger)

	router.HandleFunc("/auth/signup", userHandler.RegistrationUser).Methods(http.MethodPost).Name("Signup")
	router.HandleFunc("/auth/login", userHandler.Login).Methods(http.MethodPost).Name("Login")
	router.HandleFunc("/auth/refresh", userHandler.RefreshToken).Methods(http.MethodPost).Name("Refresh")
	router.HandleFunc("/auth/logout", userHandler.Logout).Methods(http.MethodPost)

	router.HandleFunc("/auth/{provider}/login", userHandler.OAuthLogin).Methods(http.MethodGet).Name("OAuthLogin")
	router.HandleFunc("/auth/{provider}/callback", userHandler.OAuthCallback).Methods(http.MethodGet).Name("OAuthCallback")
}