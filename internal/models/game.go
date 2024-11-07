package models

import (
	"time"

	"github.com/google/uuid"
)

type GameMode struct {
	ID          int    `db:"id"`
	Name        string `db:"name"`
	Description string `db:"description"`
}
type GameSetting struct {
	ID           int    `db:"id"`
	ModeID       int    `db:"mode_id"`
	SettingsType string `db:"settings_type"`
	GameModeID  int       `json:"game_mode_id"` 
	Value        int    `db:"value"`
	IsCustom     bool   `db:"is_custom"`
	CreatedAt       time.Time `db:"created_at"`      
}

type GameText struct {
	ID      uuid.UUID `db:"id"`    
	Content string    `db:"content"`
	Type    string    `db:"type"`    
}


type Game struct {
	ID              uuid.UUID `db:"id"`           
	UserID          uuid.UUID `db:"user_id"`        
	WPM             float64   `db:"wpm"`        
	Accuracy        float64   `db:"accuracy"`        
	DurationSeconds float64   `db:"duration_seconds"`
	CreatedAt       time.Time `db:"created_at"`      
	IsFinished      bool      `db:"is_finished"`    
	Score           int       `db:"score"`           
	SettingID       int       `db:"setting_id"`      
	TextID          uuid.UUID `db:"text_id"`         
}