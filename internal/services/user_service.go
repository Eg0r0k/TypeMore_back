package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
	"typeMore/internal/models"
	"typeMore/internal/repositories"
	"typeMore/internal/services/jwt"
	"typeMore/lib/validate"
	"typeMore/utils"

	"github.com/google/uuid"
	"github.com/markbates/goth"
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
func (s *UserService) UpsertOAuthAccount(ctx context.Context, account *models.OAuthAccount) error {
    return s.userRepo.UpsertOAuthAccount(ctx, account)
}

func (s *UserService) GetOrCreateOAuthUser(ctx context.Context, provider string, gothUser goth.User) (*models.User, error) {
    oauthAccount, err := s.userRepo.GetOAuthAccount(ctx, provider, gothUser.UserID)
    if err != nil {
        return nil, fmt.Errorf("getting oauth account: %w", err)
    }
    if oauthAccount != nil {
        return s.GetUserByID(ctx, oauthAccount.UserID)
    }
    var user *models.User
    if gothUser.Email != "" {
        user, err = s.GetUserByEmail(ctx, gothUser.Email)
        if err != nil && !errors.Is(err, sql.ErrNoRows) {
            return nil, fmt.Errorf("getting user by email: %w", err)
        }
    }

    if user == nil {
        var username string
        if gothUser.Email == "" {
            if gothUser.NickName != "" {
                username = sanitizeUsername(gothUser.NickName)
            } else if gothUser.Name != "" {
                username = sanitizeUsername(gothUser.Name)
            } else {
                username = "user-" + uuid.New().String()
            }
            gothUser.Email = fmt.Sprintf("%s@%s.com", username, provider) 
        } else {
            username = sanitizeUsername(gothUser.NickName)
        }

        user = &models.User{
            UserId:    uuid.New(),
            Username:  username,
            Email:     gothUser.Email,
            Password:  []byte{},
            AuthType:  models.AuthTypeOAuth,
            Roles:     []models.Role{models.UserRole},
        }

        if err := s.CreateUser(ctx, user, models.UserRole); err != nil {
            return nil, fmt.Errorf("creating new user: %w", err)
        }
    }

    oauthAccount = &models.OAuthAccount{
        UserID:         user.UserId,
        Provider:       provider,
        ProviderUserID: gothUser.UserID,
        Email:         gothUser.Email,
        Name:          gothUser.Name,
        AccessToken:   gothUser.AccessToken,
        RefreshToken:  gothUser.RefreshToken,
        ExpiresAt:     gothUser.ExpiresAt,
    }

    if err := s.UpsertOAuthAccount(ctx, oauthAccount); err != nil {
        return nil, fmt.Errorf("upserting oauth account: %w", err)
    }

    return user, nil
}

func sanitizeUsername(username string) string {
    reg := regexp.MustCompile("[^a-zA-Z0-9]+")
    sanitizedUsername := reg.ReplaceAllString(username, "")

    if len(sanitizedUsername) < 3 {
        sanitizedUsername = "user" 
    }

    if len(sanitizedUsername) > 20 {
        sanitizedUsername = sanitizedUsername[:20]
    }

    return strings.ToLower(sanitizedUsername)
}

func (s *UserService) GetUserByID(ctx context.Context,id uuid.UUID) (*models.User, error) {
    return s.userRepo.GetUserByID(ctx,id)
}
func (s *UserService) GetUserByEmail(ctx context.Context,email string) (*models.User, error) {
    return s.userRepo.GetUserByEmail(ctx,email)
}
func (s *UserService) CreateUser(ctx context.Context,u *models.User, role models.Role) error {
    if _,err := validate.Email(u.Email); err != nil {
        return fmt.Errorf("invalid email: %w", err)
    }
    if err := validate.ValidateUser(ctx, s.userRepo, u.Username, u.Email); err != nil {
        return err
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

func (s *UserService) RemoveRefreshToken(ctx context.Context, userID uuid.UUID, refreshToken string) error {
    return s.userRepo.DeleteRefreshToken(ctx, userID, refreshToken)
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

func (s *UserService) Login(ctx context.Context,username string, password string) (string, string, *models.User, error) {
    user, err := s.userRepo.GetUserByUsername(ctx,username)
    if err != nil {
        log.Printf("Error fetching user: %v", err)
        return "", "", nil,fmt.Errorf("error fetching user: %w", err)
    }

    if user == nil {
        log.Printf("User not found with username: %s", username)
        return "", "",nil, errors.New("invalid username or password")
    }
    err = utils.CheckPassword(user.Password, password)
    if err != nil {
        return "", "", nil, errors.New("invalid username or password")
    }
    accessToken, err := s.tokenService.GenerateAccessToken(user)
    if err != nil {
        log.Printf("Error generating access token: %v", err)
        return "", "",nil, fmt.Errorf("error generating access token: %w", err)
    }
    refreshToken, err := s.GenerateRefreshToken(ctx,user.UserId)
    if err != nil {
        log.Printf("Error generating refresh token: %v", err)
        return "", "", nil,fmt.Errorf("error generating refresh token: %w", err)
    }
 
    return accessToken, refreshToken, user, nil
}
