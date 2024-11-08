package models

import (
	"time"

	"github.com/google/uuid"
)

type PasswordResetToken struct {
	ID        uuid.UUID `json:"id"`
	UserID    uuid.UUID `json:"user_id"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Used 	  bool      `json:"used"` 	
}
type ResetPasswordRequest struct {
	Token    string `json:"token"`   
	NewPassword string `json:"new_password"` 
}