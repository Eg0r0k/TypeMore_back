package jwt

import (
	"time"
	"typeMore/internal/models"

	"github.com/google/uuid"
	"github.com/lestrrat-go/jwx/jwa"
	"github.com/lestrrat-go/jwx/jwk"
	"github.com/lestrrat-go/jwx/jwt"
)

type UserClaims struct {
    UserID uuid.UUID   `json:"user_id"`
    Roles  []models.Role `json:"roles"`
}
type Token struct {
	jwt.Token
	method jwa.SignatureAlgorithm
	secret jwk.Key
}

type TokenService struct {
    accessSecret  jwk.Key
    refreshSecret jwk.Key
    AccessTTL     time.Duration
    RefreshTTL    time.Duration
}

func NewTokenService(accessSecret, refreshSecret jwk.Key, accessTTL, refreshTTL time.Duration) *TokenService {
    return &TokenService{
        accessSecret:  accessSecret,
        refreshSecret: refreshSecret,
        AccessTTL:     accessTTL,
        RefreshTTL:    refreshTTL,
    }
}

func (s *TokenService) GenerateAccessToken(user *models.User) (string, error) {
    claims := UserClaims{
        UserID: user.UserId,
        Roles:  user.Roles,
    }

    token := jwt.New()
    token.Set(jwt.SubjectKey, claims.UserID.String())
    token.Set(jwt.ExpirationKey, time.Now().Add(s.AccessTTL))
    token.Set("roles", claims.Roles)

    signed, err := jwt.Sign(token, jwa.HS256, s.accessSecret)
    if err != nil {
        return "", err
    }

    return string(signed), nil
}

func (s *TokenService) GetRefreshTTL() time.Duration {
    return s.RefreshTTL
}
func (s *TokenService) GenerateRefreshToken(user *models.User) (string, error) {
    claims := UserClaims{
        UserID: user.UserId,
        Roles:  user.Roles,
    }

    token := jwt.New()
    token.Set(jwt.SubjectKey, claims.UserID.String())
    token.Set(jwt.ExpirationKey, time.Now().Add(s.RefreshTTL))
    token.Set("roles", claims.Roles)

    signed, err := jwt.Sign(token, jwa.HS256, s.refreshSecret)
    if err != nil {
        return "", err
    }

    return string(signed), nil
}
func (s *TokenService) ValidateAccessToken(tokenStr string) (*UserClaims, error) {
    token, err := jwt.Parse([]byte(tokenStr), jwt.WithVerify(jwa.HS256, s.accessSecret))
    if err != nil {
        return nil, err
    }

    claims := &UserClaims{
        UserID: uuid.MustParse(token.Subject()), // Получаем UserID
    }

    // Получаем роли из токена
    roles, ok := token.Get("roles")
    if ok {
        // Проверяем, что roles является списком
        if rolesList, ok := roles.([]interface{}); ok {
            // Преобразуем каждый элемент списка в строку
            for _, role := range rolesList {
                if roleStr, ok := role.(string); ok {
                    claims.Roles = append(claims.Roles, models.RoleFromString(roleStr))
                }
            }
        }
    }

    return claims, nil
}

func (s *TokenService) ValidateRefreshToken(tokenStr string) (*UserClaims, error) {
    token, err := jwt.Parse([]byte(tokenStr), jwt.WithVerify(jwa.HS256, s.refreshSecret))
    if err != nil {
        return nil, err
    }

    claims := &UserClaims{
        UserID: uuid.MustParse(token.Subject()), // Получаем UserID
    }

    // Получаем роли из токена
    roles, ok := token.Get("roles")
    if ok {
        // Проверяем, что roles является списком
        if rolesList, ok := roles.([]interface{}); ok {
            // Преобразуем каждый элемент списка в строку
            for _, role := range rolesList {
                if roleStr, ok := role.(string); ok {
                    claims.Roles = append(claims.Roles, models.RoleFromString(roleStr))
                }
            }
        }
    }

    return claims, nil
}

func (t Token) Bearer() (string, error) {
	b, err := jwt.Sign(t.Token, t.method, t.secret)
	if err != nil {
		return "", err
	}
	return string(b),nil
}



func (c *UserClaims) IsAdmin() bool {
    for _, role := range c.Roles {
        if role == models.AdminRole || role == models.SuperAdminRole {
            return true
        }
    }
    return false
}

func (c *UserClaims) NewUserToken() jwt.Token {
    token := jwt.New()
    token.Set("user_id", c.UserID.String())
    token.Set("roles", c.Roles)
    return token
}