package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
	"typeMore/internal/services"
	"typeMore/internal/services/jwt"
	"typeMore/utils"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)
type LobbyHandler struct {
    lobbyService *services.LobbyService
    tokenService *jwt.TokenService
    logger       *zap.Logger
}


func NewLobbyHandler(lobbyService *services.LobbyService, tokenService *jwt.TokenService, logger *zap.Logger) *LobbyHandler {
    return &LobbyHandler{
            lobbyService: lobbyService,
            tokenService: tokenService,
            logger:       logger,
    }
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
	
			return true
	},
}
func (h *LobbyHandler) GetOpenLobbies(w http.ResponseWriter, r *http.Request) {
    start := time.Now()
    defer func() { 
            h.logger.Info("GetOpenLobbies completed", zap.Duration("duration", time.Since(start)))
    }() 
    ctx := r.Context()
    openLobbies, err := h.lobbyService.GetOpenLobbies(ctx)
    if err != nil {
        h.logger.Error("Error retrieving open lobbies", zap.Error(err))
        utils.WriteJSONResponse(w, http.StatusInternalServerError, &utils.Response{Success: false, Error: "Error retrieving open lobbies"}) 
        return
}

utils.WriteJSONResponse(w, http.StatusOK, &utils.Response{Success: true, Data: openLobbies})
}

func (h *LobbyHandler) CreateLobby(w http.ResponseWriter, r *http.Request) {
    start := time.Now()
    defer func() {
          h.logger.Info("CreateLobby completed", zap.Duration("duration", time.Since(start)))
    }()
    var req struct {
            OwnerID    string `json:"owner_id"`
            Name       string `json:"name"`
            IsPublic   bool   `json:"is_public"`
            Password   string `json:"password"`
            MaxPlayers int    `json:"max_players"`
    }

    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
            h.logger.Error("Error decoding request body", zap.Error(err))
            utils.WriteJSONResponse(w, http.StatusBadRequest, &utils.Response{Success: false, Error: "Error decoding request body"})

            return
    }

    ownerID, err := uuid.Parse(req.OwnerID)
    if err != nil {
            h.logger.Error("Invalid owner ID", zap.Error(err), zap.String("owner_id", req.OwnerID))
            utils.WriteJSONResponse(w, http.StatusBadRequest, &utils.Response{Success: false, Error: "Invalid owner ID"})
            return
    }

    ctx := r.Context()
    lobby, err := h.lobbyService.CreateLobby(ctx, ownerID, req.Name, req.IsPublic, req.Password, req.MaxPlayers)
    if err != nil {
        h.logger.Error("Error creating lobby", zap.Error(err))
        utils.WriteJSONResponse(w, http.StatusInternalServerError, &utils.Response{Success: false, Error: "Error creating lobby"}) // Consistent error responses
        return
}

utils.WriteJSONResponse(w, http.StatusCreated, &utils.Response{Success: true, Data: lobby})
}


func (h *LobbyHandler) GetLobby(w http.ResponseWriter, r *http.Request){
    vars := mux.Vars(r)
	id, err := uuid.Parse(vars["id"])
    if err != nil {
        h.logger.Error("Invalid lobby ID", zap.Error(err), zap.String("lobby_id", vars["id"]))
        utils.WriteJSONResponse(w, http.StatusBadRequest, &utils.Response{Success: false, Error: "Invalid lobby ID"})
        return
}


	lobby, err := h.lobbyService.GetLobby(id)
    if err != nil {
        h.logger.Error("Error fetching lobby", zap.Error(err), zap.String("lobby_id", id.String()))
        utils.WriteJSONResponse(w, http.StatusInternalServerError, &utils.Response{Success: false, Error: "Error fetching lobby"})
        return
}
if lobby == nil {
    h.logger.Warn("Lobby not found", zap.String("lobby_id", id.String()))
    utils.WriteJSONResponse(w, http.StatusNotFound, &utils.Response{Success: false, Error: "Lobby not found"})
    return
}
utils.WriteJSONResponse(w, http.StatusOK, &utils.Response{Success: true, Data: lobby})
}

func (h *LobbyHandler) GetAllLobbies(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
	lobbies, err := h.lobbyService.GetAllLobbies(ctx)
	if err != nil {
        utils.WriteJSONResponse(w, http.StatusNotFound, &utils.Response{Success: false, Data: "Error fetching all lobies"})
		return
	}
    utils.WriteJSONResponse(w, http.StatusOK, &utils.Response{Success: true, Data: lobbies})

}
func (h *LobbyHandler) HandleSSELobbies(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
    flusher, ok := w.(http.Flusher)
    if !ok {
        http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
        return
    }

	lobbies, err := h.lobbyService.GetAllLobbies(ctx)
    if err != nil {
        log.Printf("Error fetching lobbies: %v", err)
        http.Error(w, "Error fetching lobbies", http.StatusInternalServerError)
        return
    }
    for _, lobby := range lobbies {
        jsonLobby, err := json.Marshal(lobby)
        if err != nil {
            log.Printf("Error marshaling lobby data: %v", err)
            continue
        }
 
        fmt.Fprintf(w, "data: %s\n\n", jsonLobby)
        flusher.Flush()
    }

    client := &services.Client{SSE: w}
    h.lobbyService.AddSSEClient(client)
    log.Println("Client connected public SSE" , )
    defer h.lobbyService.RemoveSSEClient(client)
    <-r.Context().Done()
    log.Println("Client disconnected from public SSE")
    //TODO COnnections 
}   

func (h *LobbyHandler) HandleWebSocket(w http.ResponseWriter, r *http.Request) {

    cookie, err := r.Cookie("access_token")
    if err != nil {
        http.Error(w, "Access token required", http.StatusUnauthorized)
        return
    }

    tokenStr := cookie.Value
    claims, err := h.tokenService.ValidateAccessToken(tokenStr)
    if err != nil {
        http.Error(w, "Invalid token", http.StatusUnauthorized)
        return
    }

    vars := mux.Vars(r)
    lobbyID, err := uuid.Parse(vars["id"])
    if err != nil {
        http.Error(w, "Invalid lobby ID Websocket", http.StatusBadRequest)
        return
    }

    conn, err := upgrader.Upgrade(w, r, nil)
    if err != nil { 
        h.logger.Error("Failed to upgrade to WebSocket", zap.Error(err))
        utils.WriteJSONResponse(w, http.StatusInternalServerError, &utils.Response{Success: false, Error: "Failed to upgrade to WebSocket"}) // Use consistent JSON error response
        return
}
    h.logger.Info("WebSocket connection established", zap.String("user_id", claims.UserID.String()))

    client := &services.Client{
        UserID:  claims.UserID,
        Conn:    conn,
        Send:    make(chan []byte, 256),
        LobbyID: lobbyID,
    }

    err = h.lobbyService.AddClientToLobby(lobbyID, client)
    if err != nil { 
        h.logger.Error("Error adding client to lobby (inside goroutine)", zap.Error(err))
        if client.Conn != nil {
                if err := client.Conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "Error joining lobby")); err != nil {
                        h.logger.Error("Error sending close message", zap.Error(err))
                }
                client.Conn.Close()
        }
        return
    }
  
    go func() {
        ctx := context.Background() 
        if err := h.lobbyService.AddClientToLobby(lobbyID, client); err != nil { 
            h.logger.Error("Error adding client to lobby (inside goroutine)", zap.Error(err)) 
            conn.Close()
            return
        }
        defer h.lobbyService.RemoveClientFromLobby(lobbyID, client.UserID)
        h.lobbyService.ListenClientMessages(ctx, client)
}()

    go h.lobbyService.SendClientMessages(client)
}
