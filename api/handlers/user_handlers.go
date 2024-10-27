package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
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
// @Tags Users
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
// @Tags Users
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
// @Tags Users
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
// @Tags users
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


func (h *UserHandler) OAuthLogin(w http.ResponseWriter, r *http.Request){
    provider := mux.Vars(r)["provider"]
    if provider == ""{
        h.handleError(w, fmt.Errorf("provider is required"), http.StatusBadRequest, "Provider is required")
        return
    }
    gothic.BeginAuthHandler(w, r)
}

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
//TODO: НЕ ЗАБЫТЬ СДЕЛАТЬ РЕСЕТ ПАРОЛЯ ( ДОДЕЛАТЬ ХЭНДЛЕР )
/**
* CREATE TABLE password_reset_tokens(
    id 
    email 
    token 
    expires_at 
    created_at 
)
**/
func (h *UserHandler) RequestPasswordReset(w http.Request, r *http.Request){

}