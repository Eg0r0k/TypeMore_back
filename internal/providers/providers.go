package providers

import (
	"log"
	"typeMore/utils"

	"github.com/markbates/goth"
	"github.com/markbates/goth/providers/github"
	"github.com/markbates/goth/providers/google"
)


func InitGoth() {

	googleClientID := utils.GetEnv("GOOGLE_CLIENT_ID", "----")
	googleClientSecret := utils.GetEnv("GOOGLE_CLIENT_SECRET", "-----")

	if googleClientID == "----" || googleClientSecret == "-----" {
		log.Fatal("Google Client ID and Secret must be set")
	}
	githubClientID := utils.GetEnv("GITHUB_CLIENT_ID", "----")
	githubClientSecret := utils.GetEnv("GITHUB_CLIENT_SECRET", "-----")

	if githubClientID == "----" || githubClientSecret == "-----" {
		log.Fatal("GitHub Client ID and Secret must be set")
	}

	goth.UseProviders(
		google.New(
			googleClientID,
			googleClientSecret,
			"http://localhost:3000/api/v1/auth/google/callback",
		),
		github.New(
			githubClientID,
			githubClientSecret,
			"http://localhost:3000/api/v1/auth/github/callback",
		),
	)

	
	if goth.GetProviders() == nil {
		log.Fatal("Failed to initialize Goth providers")
	}
}