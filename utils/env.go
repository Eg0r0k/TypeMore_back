package utils

import (
	"os"
	"strconv"
)

func GetEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func GetEnvAsInt(key string, fallback int) int {
	strValue := GetEnv(key, "")
	if value, err := strconv.Atoi(strValue); err == nil {
		return value
	}
	return fallback
}

func GetEnvAsBool(key string, fallback bool) bool {
	strValue := GetEnv(key, "")
	if value, err := strconv.ParseBool(strValue); err == nil {
		return value
	}
	return fallback
}