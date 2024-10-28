package api

import (
	"database/sql"

	"typeMore/api/routes"
	"typeMore/config"
	_ "typeMore/docs"
	"typeMore/internal/repositories"
	"typeMore/internal/services"
	"typeMore/internal/services/jwt"
	"typeMore/middleware"

	"github.com/gorilla/mux"
	httpSwagger "github.com/swaggo/http-swagger"
	"go.uber.org/zap"
)

func SetupRoutes(db *sql.DB, logger *zap.Logger) *mux.Router {
	router := mux.NewRouter()
	router.PathPrefix("/swagger/").Handler(httpSwagger.WrapHandler)
	router.Use(middleware.CORSMiddleware)

	// Initialize repositories
	userRepo := repositories.NewUserRepository(db)
	lobbyRepo := repositories.NewLobbyRepository(db)

	// Initialize token service
	jwtConfig := config.NewJWTConfig()
	tokenService := jwt.NewTokenService(jwtConfig.Access.Sk, jwtConfig.Refresh.Sk, jwtConfig.Access.TTL, jwtConfig.Refresh.TTL)

	// Initialize services
	userService := services.NewUserService(userRepo, tokenService)
	lobbyService := services.NewLobbyService(lobbyRepo)

	// Set up routes
	apiRouter := router.PathPrefix("/api/v1").Subrouter()
	routes.SetupAuthRoutes(apiRouter, userService, tokenService, logger)
	routes.SetupUserRoutes(apiRouter, userService, tokenService, logger)
	routes.SetupLobbyRoutes(apiRouter, lobbyService, tokenService, logger)
	routes.SetupSSERoutes(apiRouter, lobbyService)

	return router
}
