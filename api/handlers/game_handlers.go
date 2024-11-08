package handlers

import (
	"net/http"
	"typeMore/internal/services"
	"typeMore/utils"

	"go.uber.org/zap"
)

type GameHandler struct {
	gameService *services.GameService
	logger      *zap.Logger
}

func NewGameHandler(gameService *services.GameService, logger *zap.Logger) *GameHandler {
	return &GameHandler{
		gameService: gameService,
		logger:      logger,
	}
}

func (h *GameHandler) CreateGame(w http.ResponseWriter, r *http.Request) {
	utils.WriteJSONResponse(w, http.StatusOK, &utils.Response{Success: true, Data: "Game created"})
}
func (h *GameHandler) GetUserGames(w http.ResponseWriter, r *http.Request){
	utils.WriteJSONResponse(w, http.StatusOK, &utils.Response{Success: true, Data: "Game get"})
}
func (h *GameHandler) SetGameStats(w http.ResponseWriter, r *http.Request){
	utils.WriteJSONResponse(w, http.StatusOK, &utils.Response{Success: true, Data: "Game get"})

}
