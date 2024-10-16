package utils

import (
	"testing"
)
func TestHashPassword(t *testing.T) {
	password := "mySecretPassword"
	hashedPassword := HashPassword(password)
	if len(hashedPassword) == 0 {
			t.Error("hashed password should not be empty")
	}
}

func TestCheckPassword(t *testing.T) {
	password := "mySecretPassword"
	hashedPassword := HashPassword(password)

	err := CheckPassword(hashedPassword, password)
	if err != nil {
			t.Errorf("expected no error for correct password, got: %v", err)
	}

	incorrectPassword := "wrongPassword"
	err = CheckPassword(hashedPassword, incorrectPassword)
	if err == nil {
			t.Error("expected error for incorrect password, got none")
	} else {
			t.Log("correctly received error for incorrect password")
	}
}