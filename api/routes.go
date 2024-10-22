package api

import (
	"database/sql"

	"net/http"
	"typeMore/api/handlers"
	"typeMore/config"
	"typeMore/internal/repositories"
	"typeMore/internal/services"
	"typeMore/internal/services/jwt"
	"typeMore/middleware"

	_ "typeMore/docs"

	"github.com/gorilla/mux"
	httpSwagger "github.com/swaggo/http-swagger"
	"go.uber.org/zap"
)

func SetupRoutes(db *sql.DB, logger *zap.Logger) *mux.Router {
    router := mux.NewRouter()
    router.PathPrefix("/swagger/").Handler(httpSwagger.WrapHandler)
    router.Use(middleware.CORSMiddleware)

    apiRouter := router.PathPrefix("/api/v1").Subrouter() 

    
    // Create repositories
    userRepo := repositories.NewUserRepository(db)
    lobbyRepo := repositories.NewLobbyRepository(db)
    jwtConfig := config.NewJWTConfig()
    tokenService := jwt.NewTokenService(jwtConfig.Access.Sk, jwtConfig.Refresh.Sk, jwtConfig.Access.TTL, jwtConfig.Refresh.TTL)
    
    // Create services
    userService := services.NewUserService(userRepo, tokenService)
    lobbyService := services.NewLobbyService(lobbyRepo)

    // Create handlers
    userHandler := handlers.NewUserHandler(userService,tokenService,logger)
    lobbyHandler := handlers.NewLobbyHandler(lobbyService, tokenService,logger)


       // Lobby routes
    apiRouter.HandleFunc("/lobbies", lobbyHandler.CreateLobby).Methods(http.MethodPost).Name("CreateLobby")
    apiRouter.HandleFunc("/lobbies/{id:[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}}", lobbyHandler.GetLobby).Methods(http.MethodGet).Name("GetLobby")
    apiRouter.HandleFunc("/lobbies", lobbyHandler.GetAllLobbies).Methods(http.MethodGet).Name("GetAllLobbies")
    apiRouter.HandleFunc("/lobbies/open", lobbyHandler.GetOpenLobbies).Methods(http.MethodGet).Name("GetOpenLobbies")
    apiRouter.HandleFunc("/lobbies/{id}/ws", lobbyHandler.HandleWebSocket).Methods(http.MethodGet).Name("LobbyWebSocket")

   // User routes
        //??????
    apiRouter.HandleFunc("/users/{id}", userHandler.GetUser).Methods(http.MethodGet).Name("GetUser")
        //
    router.Handle("/users/{id}", middleware.TokenValidationMiddleware(tokenService)(http.HandlerFunc(userHandler.DeleteUser))).Methods("DELETE")
    apiRouter.HandleFunc("/auth/signup", userHandler.RegistrationUser).Methods(http.MethodPost).Name("Signup")
    apiRouter.HandleFunc("/auth/login", userHandler.Login).Methods(http.MethodPost).Name("Login")
    apiRouter.HandleFunc("/auth/refresh", userHandler.RefreshToken).Methods(http.MethodPost).Name("Refresh")
    // SSE route
    apiRouter.HandleFunc("/sse/public", lobbyHandler.HandleSSELobbies).Methods(http.MethodGet).Name("SSELobbies")
    return router
}