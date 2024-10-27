package validate

import (
	"fmt"
	"net/mail"
	"strings"

	"github.com/badoux/checkmail"
)

func Email(email string) (string, error) {
	e, err := mail.ParseAddress(email)
	if err != nil {
			err = fmt.Errorf("email: %w", err)
			return "", err
	}
	email = e.Address

	err = checkmail.ValidateFormat(email)
	if err != nil {
			err = fmt.Errorf("email: %w", err)
			return "", err
	}
	if !isValidDomain(email) {
			return "", fmt.Errorf("invalid domain in email address: %s", email)
	}

	return email, nil
}


func isValidDomain(email string) bool {
	parts := strings.Split(email, "@")
	if len(parts) != 2 {
			return false
	}
	domain := parts[1]
	return strings.Count(domain, ".") > 0 
}