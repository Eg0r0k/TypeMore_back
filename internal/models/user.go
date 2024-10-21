package models

import (
	"time"

	"github.com/google/uuid"
)

type User struct {
	UserId uuid.UUID `json:"id" db:"id"`

	Username string `json:"username" db:"username"`
	Email    string `json:"email" db:"email"`
	IsBanned bool   `json:"is_banned" db:"is_banned"`
	Config   string `json:"config" db:"config"`
	Password []byte `json:"-" db:"password"`

	CreatedAt        time.Time `json:"created_at:" db:"created_at"`
	UpdatedAt        time.Time `json:"updated_at" db:"updated_at"`
	LastIn           *time.Time `json:"last_in,omitempty" db:"last_in"`
    LastOut          *time.Time `json:"last_out,omitempty" db:"last_out"`
    RegistrationDate *time.Time `json:"registration_date,omitempty" db:"registration_date"`
	Roles            []Role    `db:"-"`
}

type LoginCredentials struct{
	    Username string `json:"username"`
        Password string `json:"password"`
}

type RegistrationCredentials struct{
	Username string `json:"username"`
	Email string `json:"email"`
	Password string `json:"password"`
}