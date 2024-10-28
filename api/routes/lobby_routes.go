package routes

import (
	"net/http"
	"typeMore/api/handlers"
	"typeMore/internal/services"
	"typeMore/internal/services/jwt"

	"go.uber.org/zap"

	"github.com/gorilla/mux"
)

func SetupLobbyRoutes(router *mux.Router, lobbyService *services.LobbyService, tokenService *jwt.TokenService, logger *zap.Logger) {
	lobbyHandler := handlers.NewLobbyHandler(lobbyService, tokenService, logger)

	router.HandleFunc("/lobbies", lobbyHandler.CreateLobby).Methods(http.MethodPost).Name("CreateLobby")
	router.HandleFunc("/lobbies/{id:[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}}", lobbyHandler.GetLobby).Methods(http.MethodGet).Name("GetLobby")
	router.HandleFunc("/lobbies", lobbyHandler.GetAllLobbies).Methods(http.MethodGet).Name("GetAllLobbies")
	router.HandleFunc("/lobbies/open", lobbyHandler.GetOpenLobbies).Methods(http.MethodGet).Name("GetOpenLobbies")
	router.HandleFunc("/lobbies/{id}/ws", lobbyHandler.HandleWebSocket).Methods(http.MethodGet).Name("LobbyWebSocket")
}
