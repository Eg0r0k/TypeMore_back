package routes

import (
	"net/http"
	"typeMore/api/handlers"
	"typeMore/internal/services"
	"typeMore/internal/services/jwt"
	"typeMore/middleware"

	"github.com/gorilla/mux"
	"go.uber.org/zap"
)

func SetupUserRoutes(router *mux.Router, userService *services.UserService, tokenService *jwt.TokenService, logger *zap.Logger) {
	userHandler := handlers.NewUserHandler(userService, tokenService, logger)

	router.HandleFunc("/users/{id}", userHandler.GetUser).Methods(http.MethodGet).Name("GetUser")
	router.Handle("/users/{id}", middleware.TokenValidationMiddleware(tokenService)(http.HandlerFunc(userHandler.DeleteUser))).Methods(http.MethodDelete).Name("DeleteUser")
}