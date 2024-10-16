package config

import (
	"log"
	"os"
	"time"

	"github.com/lestrrat-go/jwx/jwk"
)

type JWTConfig struct {
	Access  TokenConfig `json:"access"`
	Refresh TokenConfig `json:"refresh"`
}

type TokenConfig struct {
	Secret    string        `json:"secret"`
	Algorithm string        `json:"algorithm"`
	TTL       time.Duration `json:"ttl"`
	Sk        jwk.Key       
	Pk        jwk.Key      
}
func NewJWTConfig() *JWTConfig {
    accessSecret := os.Getenv("JWT_ACCESS_SECRET")
    refreshSecret := os.Getenv("JWT_REFRESH_SECRET")

    if accessSecret == "" || refreshSecret == "" {
        log.Fatal("JWT secret keys are not set in environment variables")
    }

    accessKey, err := jwk.New([]byte(accessSecret))
    if err != nil {
        log.Fatalf("Failed to create access key: %v", err)
    }

    refreshKey, err := jwk.New([]byte(refreshSecret))
    if err != nil {
        log.Fatalf("Failed to create refresh key: %v", err)
    }

    return &JWTConfig{
        Access: TokenConfig{
            Secret:    accessSecret,
            Algorithm: "HS256",
            TTL:       15 * time.Minute,
            Sk:        accessKey,
        },
        Refresh: TokenConfig{
            Secret:    refreshSecret,
            Algorithm: "HS256",
            TTL:       7 * 24 * time.Hour,
            Sk:        refreshKey,
        },
    }
}
