package config

import "typeMore/utils"

type Config struct {
	DBHost     string
	DBPort     int
	DBUser     string
	DBPassword string
	DBName     string
	ServerPort string
	ReloadDB   bool
}

func Load() *Config {
	return &Config{
		DBHost:     utils.GetEnv("DB_HOST", "localhost"),
		DBPort:     utils.GetEnvAsInt("DB_PORT", 5432),
		DBUser:     utils.GetEnv("DB_USER", "postgres"),
		DBPassword: utils.GetEnv("DB_PASSWORD", "admin"),
		DBName:     utils.GetEnv("DB_NAME", "TypeMore"),
		ServerPort: utils.GetEnv("SERVER_PORT", "3000"),
		ReloadDB:   utils.GetEnvAsBool("RELOAD_DB", false),
	}
}
