package models

import (
	"time"
	"github.com/google/uuid"
)

type OAuthAccount struct {
	ID             uuid.UUID `json:"id"`
	UserID         uuid.UUID `json:"user_id"`
	Provider       string    `json:"provider"`
	ProviderUserID string    `json:"provider_user_id"`
	Email          string    `json:"email"`
	Name           string    `json:"name"`
	AccessToken    string    `json:"-"`
	RefreshToken   string    `json:"-"`
	ExpiresAt      time.Time `json:"expires_at"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}
