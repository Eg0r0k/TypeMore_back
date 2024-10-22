package services

import (
	"context"
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

func (s *UserService) GetUserByID(ctx context.Context,id uuid.UUID) (*models.User, error) {
    return s.userRepo.GetUserByID(ctx,id)
}

func (s *UserService) CreateUser(ctx context.Context,u *models.User, role models.Role) error {
    if _,err := validate.Email(u.Email); err != nil {
        return fmt.Errorf("invalid email: %w", err)
    }
    taken, err := s.userRepo.IsUsernameTaken(ctx,u.Username)
    if err != nil {
            return fmt.Errorf("error checking username: %w", err)
    }
    if taken {
            return errors.New("username already taken")
    }
    
    taken, err = s.userRepo.IsEmailTaken(ctx,u.Email)
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
    u.Roles = []models.Role{role}
    err = s.userRepo.CreateUser(u)
    if err != nil {
            log.Printf("Error creating user: %v", err)
            return err
    }

    return nil
}


func (s *UserService) DeleteUser(ctx context.Context,id uuid.UUID) error{
    _, err := s.GetUserByID(ctx,id)
    if err != nil {
        return err 
    }
    return s.userRepo.DeleteUser(ctx,id)
}

func (s *UserService) GenerateRefreshToken(ctx context.Context,userID uuid.UUID) (string, error) {
    user, err := s.GetUserByID(ctx,userID)
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

    err = s.userRepo.CreateRefreshToken(ctx,token)
    if err != nil {
        return "", err
    }

    return refreshToken, nil
}

func (s *UserService) Login(ctx context.Context,username string, password string) (string, string, error) {
    user, err := s.userRepo.GetUserByUsername(ctx,username)
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
    refreshToken, err := s.GenerateRefreshToken(ctx,user.UserId)
    if err != nil {
        log.Printf("Error generating refresh token: %v", err)
        return "", "", fmt.Errorf("error generating refresh token: %w", err)
    }
 
    return accessToken, refreshToken, nil
}
