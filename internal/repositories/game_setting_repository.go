package repositories

import (
	"context"
	"database/sql"
	"errors"
	"typeMore/internal/models"
)

type GameSettingRepository struct {
	db *sql.DB
}

func NewGameSettingRepository(db *sql.DB) *GameSettingRepository {
	return &GameSettingRepository{db: db}
}
func (r *GameSettingRepository) CreateGameSetting(ctx context.Context, setting *models.GameSetting) error {
	query := `
		INSERT INTO game_settings (id, mode_id, settings_type, value, is_custom, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`
	_, err := r.db.ExecContext(ctx, query, setting.ID, setting.GameModeID, setting.SettingsType, setting.Value,
		setting.IsCustom, setting.CreatedAt)
	return err
}


func (r *GameSettingRepository) GetGameSetting(ctx context.Context, settingID int) (*models.GameSetting, error) {
	query := `
		SELECT id, mode_id, settings_type, value, is_custom, created_at
		FROM game_settings
		WHERE id = $1
	`
	setting := &models.GameSetting{}
	err := r.db.QueryRowContext(ctx, query, settingID).Scan(
		&setting.ID, &setting.GameModeID, &setting.SettingsType, &setting.Value,
		&setting.IsCustom, &setting.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, errors.New("game setting not found")
	}
	return setting, err
}

func (r *GameSettingRepository) DeleteGameSetting(ctx context.Context, settingID int) error {
	query := `DELETE FROM game_settings WHERE id = $1`
	_, err := r.db.ExecContext(ctx, query, settingID)
	return err
}