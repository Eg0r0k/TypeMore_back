package validate

import (
	"context"
	"errors"
	"fmt"
	"typeMore/internal/repositories"
)

func ValidateUser(ctx context.Context,repo *repositories.UserRepository,  username string, email string) error  {
 taken, err := repo.IsUsernameTaken(ctx, username)
 if err != nil {
	return fmt.Errorf("error checking username: %w", err)
 }
 if taken {
	return errors.New("username already taken")

 }
 taken, err = repo.IsEmailTaken(ctx, email)
 if err != nil {
	return fmt.Errorf("error checking email: %w", err)

 }
 if taken {
	return errors.New("Email alrady taken")
 }
 return nil
}