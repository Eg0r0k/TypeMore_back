package repositories

import (
	"context"
	"database/sql"
	"errors"
	"typeMore/internal/models"

	"github.com/google/uuid"
)

type GameRepository struct {
	db *sql.DB
}


func NewGameRepository(db *sql.DB) *GameRepository {
	return &GameRepository{db: db}
}


func (r *GameRepository) CreateGame(ctx context.Context, game *models.Game) error {
	query := `
		INSERT INTO games (id, user_id, wpm, accuracy, duration_seconds, created_at, is_finished, score, setting_id, text_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`
	_, err := r.db.ExecContext(ctx, query, game.ID, game.UserID, game.WPM, game.Accuracy, game.DurationSeconds,
		game.CreatedAt, game.IsFinished, game.Score, game.SettingID, game.TextID)
	return err
}

func (r *GameRepository) GetGame(ctx context.Context, gameID uuid.UUID) (*models.Game, error) {
	query := `
		SELECT id, user_id, wpm, accuracy, duration_seconds, created_at, is_finished, score, setting_id, text_id
		FROM games
		WHERE id = $1
	`
	game := &models.Game{}
	err := r.db.QueryRowContext(ctx, query, gameID).Scan(
		&game.ID, &game.UserID, &game.WPM, &game.Accuracy, &game.DurationSeconds,
		&game.CreatedAt, &game.IsFinished, &game.Score, &game.SettingID, &game.TextID,
	)
	if err == sql.ErrNoRows {
		return nil, errors.New("game not found")
	}
	return game, err
}

func (r *GameRepository) UpdateGame(ctx context.Context, game *models.Game) error {
	query := `
		UPDATE games
		SET wpm = $2, accuracy = $3, duration_seconds = $4, is_finished = $5, score = $6, updated_at = NOW()
		WHERE id = $1
	`
	_, err := r.db.ExecContext(ctx, query, game.ID, game.WPM, game.Accuracy, game.DurationSeconds, game.IsFinished, game.Score)
	return err
}


func (r *GameRepository) DeleteGame(ctx context.Context, gameID uuid.UUID) error {
	query := `DELETE FROM games WHERE id = $1`
	_, err := r.db.ExecContext(ctx, query, gameID)
	return err
}
func (r *GameRepository) GetGamesByUserID(ctx context.Context, userID uuid.UUID) ([]*models.Game, error) {
	query := `
		SELECT id, user_id, wpm, accuracy, duration_seconds, created_at, is_finished, score, setting_id, text_id
		FROM games
		WHERE user_id = $1
	`
	rows, err := r.db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var games []*models.Game
	for rows.Next() {
		game := &models.Game{}
		if err := rows.Scan(
			&game.ID, &game.UserID, &game.WPM, &game.Accuracy, &game.DurationSeconds,
			&game.CreatedAt, &game.IsFinished, &game.Score, &game.SettingID, &game.TextID,
		); err != nil {
			return nil, err
		}
		games = append(games, game)
	}
	return games, rows.Err()
}