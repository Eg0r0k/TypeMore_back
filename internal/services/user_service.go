package services

import (
	"errors"
	"fmt"
	"log"

	"time"
	"typeMore/internal/models"
	"typeMore/internal/repositories"
	"typeMore/internal/services/jwt"
	"typeMore/lib/validate"

	"typeMore/utils"

	"github.com/google/uuid"
)

type UserService struct {
    userRepo *repositories.UserRepository
    tokenService *jwt.TokenService
}

func NewUserService(userRepo *repositories.UserRepository, tokenService *jwt.TokenService) *UserService {
    return &UserService{
        userRepo:     userRepo,
        tokenService: tokenService,
    }
}

func (s *UserService) GetUserByID(id uuid.UUID) (*models.User, error) {
    return s.userRepo.GetUserByID(id)
}

func (s *UserService) CreateUser(u *models.User) error {
    if _,err := validate.Email(u.Email); err != nil {
        return fmt.Errorf("invalid email: %w", err)
    }
    taken, err := s.userRepo.IsUsernameTaken(u.Username)
    if err != nil {
            return fmt.Errorf("error checking username: %w", err)
    }
    if taken {
            return errors.New("username already taken")
    }
    
    taken, err = s.userRepo.IsEmailTaken(u.Email)
    if err != nil {
            return fmt.Errorf("error checking email: %w", err)
    }
    if taken {
            return errors.New("email already taken")
    }

    userID, err := uuid.NewV7()
    if err != nil {
            return fmt.Errorf("error generating UUID: %w", err)
    }
    u.UserId = userID
    now := time.Now()
    u.CreatedAt = now
    u.UpdatedAt = now
    u.RegistrationDate = &now
    u.Password = utils.HashPassword(string(u.Password))

    err = s.userRepo.CreateUser(u)
    if err != nil {
            log.Printf("Error creating user: %v", err)
            return err
    }

    return nil
}


func (s *UserService) DeleteUser(id uuid.UUID) error{
    _, err := s.GetUserByID(id)
    if err != nil {
        return err 
    }
    return s.userRepo.DeleteUser(id)
}

func (s *UserService) GenerateRefreshToken(userID uuid.UUID) (string, error) {
    user, err := s.GetUserByID(userID)
    if err != nil {
        return "", err
    }

    refreshToken, err := s.tokenService.GenerateRefreshToken(user)
    if err != nil {
        return "", err
    }

    token := &models.RefreshToken{
        ID:        uuid.New(),
        UserID:    userID,
        Token:     refreshToken,
        ExpiresAt: time.Now().Add(s.tokenService.GetRefreshTTL()), 
        CreatedAt: time.Now(),
    }

    err = s.userRepo.CreateRefreshToken(token)
    if err != nil {
        return "", err
    }

    return refreshToken, nil
}

func (s *UserService) Login(username string, password string) (string, string, error) {
    user, err := s.userRepo.GetUserByUsername(username)
    if err != nil {
        log.Printf("Error fetching user: %v", err)
        return "", "", fmt.Errorf("error fetching user: %w", err)
    }

    if user == nil {
        log.Printf("User not found with username: %s", username)
        return "", "", errors.New("invalid username or password")
    }
    err = utils.CheckPassword(user.Password, password)
    if err != nil {
        return "", "", errors.New("invalid username or password")
    }
    accessToken, err := s.tokenService.GenerateAccessToken(user)
    if err != nil {
        log.Printf("Error generating access token: %v", err)
        return "", "", fmt.Errorf("error generating access token: %w", err)
    }
    refreshToken, err := s.GenerateRefreshToken(user.UserId)
    if err != nil {
        log.Printf("Error generating refresh token: %v", err)
        return "", "", fmt.Errorf("error generating refresh token: %w", err)
    }
 
    return accessToken, refreshToken, nil
}
