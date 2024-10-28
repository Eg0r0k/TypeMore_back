package routes

import (
	"net/http"
	"typeMore/api/handlers"
	"typeMore/internal/services"

	"github.com/gorilla/mux"
)

func SetupSSERoutes(router *mux.Router, lobbyService *services.LobbyService) {
	lobbyHandler := handlers.NewLobbyHandler(lobbyService, nil, nil)
	router.HandleFunc("/sse/public", lobbyHandler.HandleSSELobbies).Methods(http.MethodGet).Name("SSELobbies")
}
