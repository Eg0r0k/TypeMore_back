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

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

type UserHandler struct {
    userService *services.UserService
    tokenService *jwt.TokenService
}

func NewUserHandler(userService *services.UserService, tokenService *jwt.TokenService) *UserHandler {
    return &UserHandler{userService: userService, tokenService: tokenService}
}

func (h *UserHandler) GetUser(w http.ResponseWriter, r *http.Request) {
    vars := mux.Vars(r)
    idStr := vars["id"]

    id, err := uuid.Parse(idStr)
    if err != nil {
        http.Error(w, "Invalid user ID", http.StatusBadRequest)
        return
    }

    user, err := h.userService.GetUserByID(id)
    if err != nil {
        log.Printf("Error fetching user: %v", err)
        http.Error(w, fmt.Sprintf("Error fetching user: %v", err), http.StatusInternalServerError)
        return
    }

    if user == nil {
        http.Error(w, "User not found", http.StatusNotFound)
        return
    }

    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(user)
}
//@Summary Register new user
//@Description Register a user with username, email, password and captcha
//@Tags Users
//@Accept json
//@Produce json 
//@Param user body models.User true "User data"
// @Success 201 {object} models.User
// @Router /users/register [post]
func (h *UserHandler) RegistrationUser(w http.ResponseWriter, r *http.Request){
    var newUser models.RegistrationCredentials
    err:= json.NewDecoder(r.Body).Decode(&newUser)
    if err != nil{
        http.Error(w,"Invalid request body", http.StatusBadRequest)
        return
    }
    if newUser.Username == "" || newUser.Email == "" || newUser.Password == "" {
        http.Error(w, "Username, email and password are required", http.StatusBadRequest)
        return
    }
    user := &models.User{
        Username: newUser.Username,
        Email:    newUser.Email,
        Password: []byte(newUser.Password),
    }
    err = h.userService.CreateUser(user)
    if err != nil {
        http.Error(w,"Error createing user", http.StatusInternalServerError)
        return
    }
    user.Password = nil
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusCreated)
    json.NewEncoder(w).Encode(user)
}
// @Summary Delete User
// @Description Deletes a user by ID.
// @Tags Users
// @Security ApiKeyAuth
// @Param id path string true "User ID"
// @Success 204 {string} string "User deleted successfully"
// @Failure 400 {string} string "Invalid user ID"
// @Failure 500 {string} string "Error deleting user"
// @Router /users/{id} [delete]
func (h *UserHandler) DeleteUser(w http.ResponseWriter, r *http.Request) {
    vars:= mux.Vars(r)
    idStr:= vars["id"]
    id, err:= uuid.Parse(idStr)
    if err != nil {
        http.Error(w, "Invalid user ID", http.StatusBadRequest)
        return
    }
    err = h.userService.DeleteUser(id)
    if err != nil {
        log.Printf("Error deleting user: %v", err)
        http.Error(w, fmt.Sprintf("Error deleting user: %v", err), http.StatusInternalServerError)
        return
    }
    w.WriteHeader(http.StatusNoContent)
}
// @Summary User Login
// @Description Logs in a user with username and password
// @Tags Users
// @Accept json
// @Produce json
// @Param credentials body models.LoginCredentials true "Login credentials"  
// @Success 200 {object} map[string]string
// @Failure 400 {string} string "Invalid request payload"
// @Failure 401 {string} string "Invalid username or password"
// @Router /users/login [post]
func (h *UserHandler)  Login(w http.ResponseWriter, r *http.Request) {
    var creds models.LoginCredentials
    
    if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
        http.Error(w, "Invalid request payload", http.StatusBadRequest)
        return
    }
    accessToken, refreshToken, err := h.userService.Login(creds.Username, creds.Password)
    if err != nil {
            http.Error(w, "Invalid username or password", http.StatusUnauthorized)
            return
    }
    http.SetCookie(w, &http.Cookie{
        Name:     "access_token",
        Value:    accessToken,
        Path:     "/",
        Expires:  time.Now().Add(h.tokenService.AccessTTL),
        HttpOnly: true,
        Secure:   true,
        SameSite: http.SameSiteStrictMode,
    })
    http.SetCookie(w, &http.Cookie{
        Name:     "refresh_token",
        Value:    refreshToken,
        Path:     "/",
        Expires:  time.Now().Add(h.tokenService.RefreshTTL), 
        HttpOnly: true,
        Secure:   true,
        SameSite: http.SameSiteStrictMode,
    })
    response := map[string]string{
        "access_token":  accessToken,
        "refresh_token": refreshToken,
}

w.Header().Set("Content-Type", "application/json")
json.NewEncoder(w).Encode(response)
}

func (h *UserHandler) RefreshToken(w http.ResponseWriter, r *http.Request) {
    var request struct {
        RefreshToken string `json:"refresh_token"`
    }

    if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
        http.Error(w, "Invalid request payload", http.StatusBadRequest)
        return
    }

    claims, err := h.tokenService.ValidateRefreshToken(request.RefreshToken)
    if err != nil {
        http.Error(w, "Invalid refresh token", http.StatusUnauthorized)
        return
    }

    user, err := h.userService.GetUserByID(claims.UserID)
    if err != nil {
        http.Error(w, "User not found", http.StatusUnauthorized)
        return
    }

    newAccessToken, err := h.tokenService.GenerateAccessToken(user)
    if err != nil {
        http.Error(w, "Could not generate access token", http.StatusInternalServerError)
        return
    }


    newRefreshToken, err := h.userService.GenerateRefreshToken(user.UserId)
    if err != nil {
        http.Error(w, "Could not generate refresh token", http.StatusInternalServerError)
        return
    }

    response := map[string]string{
        "access_token":  newAccessToken,
        "refresh_token": newRefreshToken,
    }
    http.SetCookie(w, &http.Cookie{
        Name:     "access_token",
        Value:    newAccessToken,
        Path:     "/",
        Expires:  time.Now().Add(h.tokenService.AccessTTL),
        HttpOnly: true,
        Secure:   true,
        SameSite: http.SameSiteStrictMode,
    })

    http.SetCookie(w, &http.Cookie{
        Name:     "refresh_token",
        Value:    newRefreshToken,
        Path:     "/",
        Expires:  time.Now().Add(h.tokenService.RefreshTTL), 
        HttpOnly: true,
        Secure:   true,
        SameSite: http.SameSiteStrictMode,
    })
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(response)
}