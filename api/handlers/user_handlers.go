package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
	"typeMore/internal/messaging"
	"typeMore/internal/models"
	"typeMore/internal/services"
	"typeMore/internal/services/jwt"
	"typeMore/utils"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/markbates/goth/gothic"
	"go.uber.org/zap"
)

type UserHandler struct {
    userService *services.UserService
    tokenService *jwt.TokenService
    logger       *zap.Logger
}

func NewUserHandler(userService *services.UserService, tokenService *jwt.TokenService, logger *zap.Logger) *UserHandler {
    return &UserHandler{userService: userService, tokenService: tokenService, logger: logger}
}

func (h *UserHandler) setTokensInCookies(w http.ResponseWriter, accessToken string, refreshToken string) {
	utils.SetCookie(w, "access_token", accessToken, "/", h.tokenService.AccessTTL, true, true, http.SameSiteStrictMode)
	utils.SetCookie(w, "refresh_token", refreshToken, "/", h.tokenService.RefreshTTL, true, true, http.SameSiteStrictMode)
}

// GetUser handler with improved logging and error handling
// @Summary Get User by ID
// @Description Retrieves a user by their ID.
// @Tags Users
// @Produce json
// @Param id path string true "User ID"
// @Success 200 {object} models.User
// @Failure 400 {string} string "Invalid user ID"
// @Failure 404 {string} string "User not found"
// @Failure 500 {string} string "Error fetching user"
// @Router /api/v1/users/{id} [get] 
func (h *UserHandler) GetUser(w http.ResponseWriter, r *http.Request) {
    start := time.Now()
    ctx := r.Context()
    vars := mux.Vars(r)
    idStr := vars["id"]
    defer func() {
            h.logger.Info("GetUser completed", zap.Duration("duration", time.Since(start)), zap.String("user_id", mux.Vars(r)["id"]))
    }()
    id, err := uuid.Parse(idStr)
    if err != nil {
        h.logger.Error("Invalid user ID", zap.Error(err), zap.String("id", idStr))
        utils.WriteJSONResponse(w, http.StatusBadRequest, &utils.Response{Success: false, Error: "Invalid user ID"})

        return
    }
    user, err := h.userService.GetUserByID(ctx,id)
    if err != nil {
        h.logger.Error("Error fetching user", zap.Error(err), zap.String("user_id", id.String()))
        utils.WriteJSONResponse(w, http.StatusInternalServerError, &utils.Response{Success: false, Error: "Error fetching user"})
        return
    }
    if user == nil {
        h.logger.Warn("User not found", zap.String("user_id", id.String()))
        utils.WriteJSONResponse(w, http.StatusNotFound, &utils.Response{Success: false, Error: "User not found"})

        return
    }
    utils.WriteJSONResponse(w, http.StatusOK, &utils.Response{Success: true, Data: user})

}

// @Summary Register new user
// @Description Register a user with username, email, and password.
// @Tags Auth
// @Accept json
// @Produce json
// @Param user body models.RegistrationCredentials true "User data"
// @Success 201 {object} models.User
// @Failure 400 {string} string "Invalid request body"
// @Failure 500 {string} string "Error creating user"
// @Router /api/v1/auth/signup [post] 
func (h *UserHandler) RegistrationUser(w http.ResponseWriter, r *http.Request){
    start := time.Now()
    ctx := r.Context()
    defer func() {
            h.logger.Info("RegistrationUser completed", zap.Duration("duration", time.Since(start)))
    }()
    var newUser models.RegistrationCredentials
    err:= json.NewDecoder(r.Body).Decode(&newUser)
    if err != nil{
        h.logger.Error("Invalid request body", zap.Error(err))
        http.Error(w, "Invalid request body", http.StatusBadRequest)
        utils.WriteJSONResponse(w, http.StatusBadRequest, &utils.Response{Success: false, Error: "Invalid request body"})
        return
    }
    if newUser.Username == "" || newUser.Email == "" || newUser.Password == "" {
        utils.WriteJSONResponse(w, http.StatusBadRequest, &utils.Response{Success: false, Error: "Username, email and password are required"})
        return
    }
    user := &models.User{
        Username: newUser.Username,
        Email:    newUser.Email,
        Password: []byte(newUser.Password),
    }
    role := models.UserRole 
    err = h.userService.CreateUser(ctx,user,role)
    if err != nil {
        utils.WriteJSONResponse(w, http.StatusBadRequest, &utils.Response{Success: false, Error: "Error createing user already exist"})
        return
    }
    user.Password = nil
    utils.WriteJSONResponse(w, http.StatusCreated, &utils.Response{Success: true, Data: user})
}

// @Summary Delete User
// @Description Deletes a user by ID.
// @Tags Users
// @Security ApiKeyAuth 
// @Param id path string true "User ID"
// @Success 204 "User deleted successfully"
// @Failure 400 {string} string "Invalid user ID"
// @Failure 500 {string} string "Error deleting user"
// @Router /api/v1/users/{id} [delete]
func (h *UserHandler) DeleteUser(w http.ResponseWriter, r *http.Request) {
    vars:= mux.Vars(r)
    ctx := r.Context()
    idStr:= vars["id"]
    id, err:= uuid.Parse(idStr)
    if err != nil {
        utils.WriteJSONResponse(w, http.StatusBadRequest, &utils.Response{Success: false, Error: "Invalid user ID"})
        return
    }
    err = h.userService.DeleteUser(ctx,id)
    if err != nil {
        log.Printf("Error deleting user: %v", err)
    
        utils.WriteJSONResponse(w, http.StatusInternalServerError, &utils.Response{Success: false, Error: fmt.Sprintf("Error deleting user: %v", err)})

        return
    }
    w.WriteHeader(http.StatusNoContent)
}
// @Summary User Login
// @Description Logs in a user with username and password.
// @Tags Auth
// @Accept json
// @Produce json
// @Param credentials body models.LoginCredentials true "Login credentials"
// @Success 200 {object} map[string]string
// @Failure 400 {string} string "Invalid request payload"
// @Failure 401 {string} string "Invalid username or password"
// @Router /api/v1/auth/login [post] 
func (h *UserHandler)  Login(w http.ResponseWriter, r *http.Request) {
    start := time.Now()
    ctx := r.Context()
    defer func() {
        h.logger.Info("Login completed", zap.Duration("duration", time.Since(start)))
    }()
    var creds models.LoginCredentials
    if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
        utils.WriteJSONResponse(w, http.StatusBadRequest, &utils.Response{Success: false, Error: "Invalid request payload"})
        return
    }
    accessToken, refreshToken, user, err := h.userService.Login(ctx, creds.Username, creds.Password) 
    if err != nil {
        h.logger.Error("Login failed", zap.Error(err), zap.String("username", creds.Username))
        utils.WriteJSONResponse(w, http.StatusUnauthorized, &utils.Response{Success: false, Error: "Invalid username or password"})
        return
    }
    h.setTokensInCookies(w, accessToken, refreshToken)

    responseData := struct {
        AccessToken  string      `json:"access_token"`
        RefreshToken string      `json:"refresh_token"`
        User         *models.User `json:"user"`
    }{
        AccessToken:  accessToken,
        RefreshToken: refreshToken,
        User:         user,
    }
utils.WriteJSONResponse(w, http.StatusOK, &utils.Response{Success: true, Data: responseData})
}

// @Summary Refresh Access Token
// @Description Refreshes the access token using a refresh token.
// @Tags Auth
// @Accept json
// @Produce json
// @Param refresh_token body string true "Refresh token"
// @Success 200 {object} map[string]string
// @Failure 400 {string} string "Invalid request payload
// @Router /api/v1/auth/refresh [post] 
func (h *UserHandler) RefreshToken(w http.ResponseWriter, r *http.Request) {
    start := time.Now()
    ctx := r.Context()
    defer func() {
        duration := time.Since(start)
        h.logger.Info("RefreshToken completed", zap.Duration("duration", duration))
    }()
    var request struct {
        RefreshToken string `json:"refresh_token"`
    }
    if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
        utils.WriteJSONResponse(w, http.StatusBadRequest, &utils.Response{Success: false, Error: "Invalid request payload"})
        return
    }
    claims, err := h.tokenService.ValidateRefreshToken(request.RefreshToken)
    if err != nil {
        h.logger.Error("Invalid refresh token", zap.Error(err)) 
        utils.WriteJSONResponse(w, http.StatusUnauthorized, &utils.Response{Success: false, Error: "Invalid refresh token"})
        return
    }
    user, err := h.userService.GetUserByID(ctx,claims.UserID)
    if err != nil {
        utils.WriteJSONResponse(w, http.StatusUnauthorized, &utils.Response{Success: false, Error: "User not found"})
        return
    }
    newAccessToken, err := h.tokenService.GenerateAccessToken(user)
    if err != nil {
        utils.WriteJSONResponse(w, http.StatusInternalServerError, &utils.Response{Success: false, Error: "Could not generate access token"})
        return
    }

    newRefreshToken, err := h.userService.GenerateRefreshToken(ctx,user.UserId)
    if err != nil {
        utils.WriteJSONResponse(w, http.StatusInternalServerError, &utils.Response{Success: false, Error: "Could not generate refresh token"})
        return
    }
    response := map[string]string{
        "access_token":  newAccessToken,
        "refresh_token": newRefreshToken,
    }
    h.setTokensInCookies(w, newAccessToken, newRefreshToken)
    utils.WriteJSONResponse(w, http.StatusOK, &utils.Response{Success: true, Data: response})
}
func (h *UserHandler) handleError(w http.ResponseWriter, err error, status int, message string) {
    h.logger.Error(message, zap.Error(err))
    utils.WriteJSONResponse(w, status, &utils.Response{Success: false, Error: message})
}
// @Summary Logout user
// @Description Logs out the user by invalidating their access and refresh tokens.
// @Tags Auth
// @Success 200 {object} utils.Response
// @Failure 204 {object} utils.Response "No content, token not found"
// @Failure 401 {object} utils.Response "Unauthorized, invalid token"
// @Failure 500 {object} utils.Response "Internal server error, failed to remove refresh token"
// @Router /api/v1/auth/logout [post] 
func (h *UserHandler) Logout(w http.ResponseWriter, r *http.Request) {
    start := time.Now()
    ctx := r.Context()
    defer func() {
        duration := time.Since(start)
        h.logger.Info("Logout completed", zap.Duration("duration", duration))
    }()
    accessToken, err := r.Cookie("access_token")
    if err != nil {
        h.handleError(w, err, http.StatusNoContent, "Access token not found")
        return
    }    
    tokenStr := accessToken.Value
    claims, err := h.tokenService.ValidateAccessToken(tokenStr)
    if err != nil {
        http.Error(w, "Invalid token", http.StatusUnauthorized)
        return
    }
    refreshToken, err := r.Cookie("refresh_token")
    if err != nil {
        h.handleError(w, err, http.StatusNoContent, "Refresh token not found")
        return
    }
    err = h.userService.RemoveRefreshToken(ctx, claims.UserID, refreshToken.Value)
    if err != nil {
        h.handleError(w, err, http.StatusInternalServerError, "Failed to remove refresh token")
        return
    }

    utils.ClearCookie(w, "access_token")
    utils.ClearCookie(w, "refresh_token")

    w.WriteHeader(http.StatusOK)
}

// @Summary Initiates OAuth login process.
// @Description Starts the OAuth login flow for a given provider.
// @Tags Auth
// @Param provider path string true "OAuth provider name" Enum("google", "github")
// @Success 302 "Redirects to the OAuth provider's authorization page"
// @Failure 400 {object} utils.Response "Provider is required"
// @Router /api/v1/auth/{provider}/login [get] 
func (h *UserHandler) OAuthLogin(w http.ResponseWriter, r *http.Request){
    provider := mux.Vars(r)["provider"]
    if provider == ""{
        h.handleError(w, fmt.Errorf("provider is required"), http.StatusBadRequest, "Provider is required")
        return
    }
    gothic.BeginAuthHandler(w, r)
}
// @Summary Handles OAuth callback after provider authentication.
// @Description Completes OAuth flow, retrieves user data, creates or updates user account, and returns tokens.
// @Tags Auth
// @Param provider path string true "OAuth provider name" Enum("google", "github")
// @Success 200 {object} utils.Response "User data and tokens"
// @Failure 500 {object} utils.Response "Failed to process authentication"
// @Router /api/v1/auth/{provider}/callback [get] 
func (h *UserHandler) OAuthCallback(w http.ResponseWriter, r *http.Request) {
    start := time.Now()
    ctx := r.Context()
    defer func() {
        h.logger.Info("OAuthCallback completed",
            zap.Duration("duration", time.Since(start)),
            zap.String("provider", mux.Vars(r)["provider"]))
    }()

    provider := mux.Vars(r)["provider"]
    h.logger.Info("Processing OAuth callback", zap.String("url", r.URL.String()))

    gothUser, err := gothic.CompleteUserAuth(w, r)
    if err != nil {
        h.logger.Error("Error completing user auth", zap.Error(err))
        h.handleError(w, err, http.StatusInternalServerError, "Failed to complete authentication")
        return
    }

    dbUser, err := h.userService.GetOrCreateOAuthUser(ctx, provider, gothUser)
    if err != nil {
        h.logger.Error("Failed to get or create OAuth user",
            zap.Error(err),
            zap.String("email", gothUser.Email),
            zap.String("provider", provider))
        h.handleError(w, err, http.StatusInternalServerError, "Failed to process user account")
        return
    }
    oauthAccount := &models.OAuthAccount{
        UserID:         dbUser.UserId,
        Provider:       provider,
        ProviderUserID: gothUser.UserID,
        Email:         gothUser.Email,
        Name:          gothUser.Name,
        AccessToken:   gothUser.AccessToken,
        RefreshToken:  gothUser.RefreshToken,
        ExpiresAt:     gothUser.ExpiresAt,
    }
    if err := h.userService.UpsertOAuthAccount(ctx, oauthAccount); err != nil {
        h.logger.Error("Failed to upsert OAuth account",
            zap.Error(err),
            zap.String("user_id", dbUser.UserId.String()),
            zap.String("provider", provider))
        h.handleError(w, err, http.StatusInternalServerError, "Failed to update OAuth account")
        return
    }
    accessToken, err := h.tokenService.GenerateAccessToken(dbUser)
    if err != nil {
        h.handleError(w, err, http.StatusInternalServerError, "Failed to generate access token")
        return
    }
    refreshToken, err := h.userService.GenerateRefreshToken(ctx, dbUser.UserId)
    if err != nil {
        h.handleError(w, err, http.StatusInternalServerError, "Failed to generate refresh token")
        return
    }
    h.setTokensInCookies(w, accessToken, refreshToken)
    dbUser.Password = nil 
    utils.WriteJSONResponse(w, http.StatusOK, &utils.Response{
        Success: true,
        Data: map[string]interface{}{
            "access_token":  accessToken,
            "refresh_token": refreshToken,
            "user":         dbUser,
        },
    })
}
// @Summary Requests password reset for a user.
// @Description Sends a password reset token to the user's email if the account exists.
// @Tags Auth
// @Param email body string true "User's email for password reset"
// @Success 200 {object} utils.Response "Password reset email sent"
// @Failure 400 {object} utils.Response "Invalid request payload"
// @Failure 404 {object} utils.Response "User not found"
// @Failure 500 {object} utils.Response "Failed to generate token or send email"
// @Router /api/v1/auth/request_password_reset [post] 
func (h *UserHandler) RequestPasswordReset(w http.ResponseWriter, r *http.Request) {
    var payload struct {
        Email string `json:"email"`
    }
    if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
        utils.WriteJSONResponse(w, http.StatusBadRequest, &utils.Response{Success: false, Error: "Invalid request payload"})
        return
    }
    user, err := h.userService.GetUserByEmail(r.Context(), payload.Email)
    if err != nil || user == nil {
        utils.WriteJSONResponse(w, http.StatusNotFound, &utils.Response{Success: false, Error: "User not found"})
        return
    }
    token, err := utils.GenerateRandomToken()
    if err != nil {
        h.logger.Error("Failed to generate random token", zap.Error(err))
        utils.WriteJSONResponse(w, http.StatusInternalServerError, &utils.Response{Success: false, Error: "Failed to generate token"})
        return
    }
    expiresAt := time.Now().Add(15 * time.Minute)
    if err := h.userService.SavePasswordResetToken(r.Context(), user.UserId, token, expiresAt); err != nil {
        h.logger.Error("Failed to save password reset token", zap.Error(err))
        utils.WriteJSONResponse(w, http.StatusInternalServerError, &utils.Response{Success: false, Error: "Failed to process request"})
        return
    }
    if err := messaging.SendEmail(messaging.EmailMessage{
        To:      user.Email,
        Subject: "Password Reset Request",
        Body:    fmt.Sprintf("Your password reset token is: %s", token),
    }); err != nil {
        h.logger.Error("Failed to send email", zap.Error(err))
        utils.WriteJSONResponse(w, http.StatusInternalServerError, &utils.Response{Success: false, Error: "Failed to send email"})
        return
    }
    utils.WriteJSONResponse(w, http.StatusOK, &utils.Response{Success: true, Data: "Password reset email sent"})

}
// @Summary Resets user's password.
// @Description Verifies the reset token, resets the password, and clears the reset token.
// @Tags Auth
// @Param body body models.ResetPasswordRequest true "New password and reset token"
// @Success 200 {object} utils.Response "Password reset successfully"
// @Failure 400 {object} utils.Response "Invalid request payload or expired token"
// @Failure 404 {object} utils.Response "Invalid or expired token"
// @Failure 500 {object} utils.Response "Failed to reset password"
// @Router /api/v1/auth/reset_password [post] 
func (h *UserHandler) ResetPassword(w http.ResponseWriter, r *http.Request) {
    var payload models.ResetPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		utils.WriteJSONResponse(w, http.StatusBadRequest, &utils.Response{Success: false, Error: "Invalid request payload"})
		return
	}
    resetToken, err := h.userService.GetPasswordResetTokenByToken(r.Context(), payload.Token)
    if err != nil || resetToken == nil {
        utils.WriteJSONResponse(w, http.StatusNotFound, &utils.Response{Success: false, Error: "Invalid or expired token"})
        return
    }

    if time.Now().After(resetToken.ExpiresAt) {
        utils.WriteJSONResponse(w, http.StatusBadRequest, &utils.Response{Success: false, Error: "Token has expired"})
        return
    }
    user, err := h.userService.GetUserByID(r.Context(), resetToken.UserID)
    if err != nil || user == nil {
        utils.WriteJSONResponse(w, http.StatusInternalServerError, &utils.Response{Success: false, Error: "User not found"})
        return
    }
    if err := h.userService.UpdateUserPassword(r.Context(), resetToken.UserID, payload.NewPassword, payload.Token); err != nil {
        h.logger.Error("Failed to update user password", zap.Error(err))
        utils.WriteJSONResponse(w, http.StatusInternalServerError, &utils.Response{Success: false, Error: "Failed to reset password"})
        return
    }

    if err := h.userService.ClearResetToken(r.Context(), payload.Token); err != nil {
        h.logger.Error("Failed to clear reset token", zap.Error(err))
    }
    emailMessage := messaging.EmailMessage{
        To:      user.Email, 
        Subject: "Password Reset Confirmation",
        Body:    "Your password has been successfully reset.",
    }
    if err := messaging.SendEmail(emailMessage); err != nil {
		h.logger.Error("Failed to send confirmation email", zap.Error(err))
	}
    utils.WriteJSONResponse(w, http.StatusOK, &utils.Response{Success: true, Data: "Password reset successfully"})

}