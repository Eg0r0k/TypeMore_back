package routes

import (
	"net/http"
	"typeMore/api/handlers"
	"typeMore/internal/services"

	"github.com/gorilla/mux"
	"go.uber.org/zap"
)

func SetupGameRoutes(router *mux.Router, gameService *services.GameService, logger *zap.Logger ){
	gameHandler := handlers.NewGameHandler(gameService, logger)
	router.HandleFunc("/start", gameHandler.CreateGame).Methods(http.MethodPost).Name("CreateGame")
	router.HandleFunc("/games", gameHandler.GetUserGames).Methods(http.MethodGet).Name("GetUserGames")
	router.HandleFunc("/stats", gameHandler.SetGameStats).Methods(http.MethodPost).Name("SetGameStats")
}