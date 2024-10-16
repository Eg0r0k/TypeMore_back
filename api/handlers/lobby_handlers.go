package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"typeMore/internal/services"
	"typeMore/internal/services/jwt"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)
type LobbyHandler struct {
	lobbyService *services.LobbyService
	tokenService *jwt.TokenService
}

func NewLobbyHandler(lobbyService *services.LobbyService, tokenService *jwt.TokenService) *LobbyHandler {
	return &LobbyHandler{lobbyService: lobbyService, tokenService: tokenService}
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
	
			return true
	},
}

func (h *LobbyHandler) CreateLobby(w http.ResponseWriter, r *http.Request){
    var req struct {
        OwnerID    string `json:"owner_id"`
        Name       string `json:"name"`
        IsPublic   bool   `json:"is_public"`
        Password   string `json:"password"`
        MaxPlayers int    `json:"max_players"`
    }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
	ownerID, err := uuid.Parse(req.OwnerID)
    if err != nil {
        http.Error(w, "Invalid owner ID", http.StatusBadRequest)
        return
    }
	lobby, err := h.lobbyService.CreateLobby(ownerID, req.Name, req.IsPublic, req.Password, req.MaxPlayers)
	if err != nil {
		log.Printf("Error creating lobby: %v", err)
		http.Error(w, fmt.Sprintf("Lobby Create error: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(lobby)

}

func (h *LobbyHandler) GetLobby(w http.ResponseWriter, r *http.Request){
    vars := mux.Vars(r)
	id, err := uuid.Parse(vars["id"])
	if err != nil {
        http.Error(w, "Invalid lobby ID", http.StatusBadRequest)
        return
    }

	lobby, err := h.lobbyService.GetLobby(id)
	if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
	w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(lobby)
}

func (h *LobbyHandler) GetAllLobbies(w http.ResponseWriter, r *http.Request) {
	lobbies, err := h.lobbyService.GetAllLobbies()
	if err != nil {
		http.Error(w , err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type","application/json")
	json.NewEncoder(w).Encode(lobbies)

}


func (h *LobbyHandler) HandleWebSocket(w http.ResponseWriter, r *http.Request) {

    // Получение куки с токеном
    cookie, err := r.Cookie("access_token")
    if err != nil {
        http.Error(w, "Access token required", http.StatusUnauthorized)
        return
    }

    // Валидация токена
    tokenStr := cookie.Value
    claims, err := h.tokenService.ValidateAccessToken(tokenStr)
    if err != nil {
        http.Error(w, "Invalid token", http.StatusUnauthorized)
        return
    }

    // Получаем ID лобби из URL
    vars := mux.Vars(r)
    lobbyID, err := uuid.Parse(vars["id"])
    if err != nil {
        http.Error(w, "Invalid lobby ID", http.StatusBadRequest)
        return
    }

    // Устанавливаем WebSocket-соединение
    conn, err := upgrader.Upgrade(w, r, nil)
    if err != nil {
        log.Println("Failed to upgrade to WebSocket:", err)
        return
    }

    log.Printf("WebSocket connection established for user %s", claims.UserID.String())

    // Создание клиента и добавление его в лобби
    client := &services.Client{
        UserID:  claims.UserID,
        Conn:    conn,
        Send:    make(chan []byte),
        LobbyID: lobbyID,
    }

    err = h.lobbyService.AddClientToLobby(lobbyID, client)
    if err != nil {
        log.Printf("Error adding client to lobby: %v", err)
        conn.Close()
        return
    }

    // Запуск горутин для обработки сообщений от клиента и отправки сообщений клиенту
    go h.lobbyService.ListenClientMessages(*client)
    go h.lobbyService.SendClientMessages(*client)
}
