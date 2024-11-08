package services

import "typeMore/internal/repositories"

type GameService struct {
	gameRepo *repositories.GameRepository
}

func NewGameService(gameRepo *repositories.GameRepository) *GameService{
	return &GameService{
		gameRepo: gameRepo,
	}
}

