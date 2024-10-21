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
)

func SetupRoutes(db *sql.DB) *mux.Router {
    router := mux.NewRouter()
    router.PathPrefix("/swagger/").Handler(httpSwagger.WrapHandler)



    // Create repositories
    userRepo := repositories.NewUserRepository(db)
    lobbyRepo := repositories.NewLobbyRepository(db)
    jwtConfig := config.NewJWTConfig()
    tokenService := jwt.NewTokenService(jwtConfig.Access.Sk, jwtConfig.Refresh.Sk, jwtConfig.Access.TTL, jwtConfig.Refresh.TTL)
    
    // Create services
    userService := services.NewUserService(userRepo, tokenService)
    lobbyService := services.NewLobbyService(lobbyRepo)

    // Create handlers
    // authHandler := handlers.NewAuthHandler(userService)
    userHandler := handlers.NewUserHandler(userService,tokenService)
    lobbyHandler := handlers.NewLobbyHandler(lobbyService, tokenService)
    // lobbyHandler := handlers.NewLobbyHandler(db)

    // Auth routes
    // router.HandleFunc("/auth/login", authHandler.Login).Methods("POST")
    // router.HandleFunc("/auth/logout", authHandler.Logout).Methods("POST")
    // router.HandleFunc("/auth/register", authHandler.Register).Methods("POST")

    // User routes
    // router.HandleFunc("/users", userHandler.ListUsers).Methods("GET")
    router.HandleFunc("/users/{id}", userHandler.GetUser).Methods("GET")
    router.HandleFunc("/user/signup", userHandler.RegistrationUser).Methods("POST")
    router.HandleFunc("/lobbies", lobbyHandler.CreateLobby).Methods("POST")
    router.HandleFunc("/lobbies/{id}", lobbyHandler.GetLobby).Methods("GET")
    router.HandleFunc("/lobbies", lobbyHandler.GetAllLobbies).Methods("GET")
	router.Handle("/users/{id}", middleware.TokenValidationMiddleware(tokenService)(http.HandlerFunc(userHandler.DeleteUser))).Methods("DELETE")

    router.HandleFunc("/users/login", userHandler.Login).Methods("POST")
    router.HandleFunc("/auth/refresh", userHandler.RefreshToken).Methods("POST")
    // router.HandleFunc("/users/{id}", userHandler.UpdateUser).Methods("PUT", "PATCH")
    // router.HandleFunc("/users/{id}", userHandler.DeleteUser).Methods("DELETE")
    // router.HandleFunc("/users/{id}/statistics", userHandler.GetUserStatistics).Methods("GET")

    // Lobby routes
    // router.HandleFunc("/lobbies", lobbyHandler.ListLobbies).Methods("GET")
    // router.HandleFunc("/lobbies/{id}", lobbyHandler.GetLobby).Methods("GET")
    // router.HandleFunc("/lobbies/{id}", lobbyHandler.UpdateLobby).Methods("PUT", "PATCH")
    // router.HandleFunc("/lobbies/{id}", lobbyHandler.DeleteLobby).Methods("DELETE")
    // router.HandleFunc("/lobbies/ws", lobbyHandler.HandleWebSocket)
    router.HandleFunc("/lobbies/{id}/ws", lobbyHandler.HandleWebSocket).Methods("GET")

    return router
}