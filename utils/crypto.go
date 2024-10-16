package utils

import "golang.org/x/crypto/bcrypt"

func HashPassword(password string) []byte {
	pw, _ := bcrypt.GenerateFromPassword([]byte(password),bcrypt.DefaultCost)
	return pw
}



func CheckPassword(hashedPassword []byte, password string) error {
    return bcrypt.CompareHashAndPassword(hashedPassword, []byte(password))
}