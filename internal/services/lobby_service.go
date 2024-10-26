package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
	"typeMore/internal/models"
	"typeMore/internal/repositories"
	"typeMore/utils"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
			// Implement appropriate origin checking logic here
			// For development, you might allow all origins, but in production, restrict it.
			// Example:
			// allowedOrigin := "https://yourdomain.com"
			// return r.Header.Get("Origin") == allowedOrigin
			return true // Allow all for now - CHANGE IN PRODUCTION
	},
}


type Client struct {
	SSE http.ResponseWriter
	UserID  uuid.UUID
	Conn    *websocket.Conn
	Send    chan []byte
	LobbyID uuid.UUID
	mu sync.Mutex
}
type LobbyService struct {
	lobbyRepo *repositories.LobbyRepository
	clients   map[uuid.UUID]map[uuid.UUID]*Client 
	mu        sync.RWMutex   
	sseClients   []*Client
	sseClientsMu sync.Mutex
}
func NewLobbyService(lobbyRepo *repositories.LobbyRepository) *LobbyService {
	return &LobbyService{
			lobbyRepo: lobbyRepo,
			clients:   make(map[uuid.UUID]map[uuid.UUID]*Client),
	}
}
func (s *LobbyService) CreateLobby(ctx context.Context, ownerID uuid.UUID, name string, isPublic bool, password string, maxPlayers int) (*models.Lobby, error) {
	lobbyID, err := uuid.NewV7() 
	if err != nil {
			return nil, err
	}

	hashedPassword := []byte(nil)
	if !isPublic && password != "" {
			hashedPassword = utils.HashPassword(password) 
	}

	newLobby := &models.Lobby{
			LobbyID:    lobbyID,
			CreateAt:   time.Now(),
			UpdatedAt:   time.Now(),
			IsPublic:   isPublic,
			Password:   hashedPassword,
			Status:     models.Active,
			OwnerID:    ownerID,
			MaxPlayers: maxPlayers,
			Name:       name,
			IsOpen:     true,
			Players:    []uuid.UUID{ownerID},
	}

	err = s.lobbyRepo.CreateLobby(ctx, newLobby)
	if err != nil {
			return nil, err
	}

	s.mu.Lock()
	s.clients[lobbyID] = make(map[uuid.UUID]*Client) 
	s.mu.Unlock()
    s.broadcastLobbyUpdate(models.LobbyCreated, newLobby)
    return newLobby, nil
}
func (s *LobbyService) broadcastLobbyUpdate(updateType models.LobbyUpdateType, lobby *models.Lobby) {
    s.mu.RLock()
    defer s.mu.RUnlock()

    updateMessage := &models.LobbyUpdateMessage{
        Type:  updateType,
        Lobby: lobby,
    }

    messageBytes, err := json.Marshal(updateMessage)
    if err != nil {
        log.Printf("Error marshalling lobby update: %v", err)
        return
    }

    // Отправляем обновление всем SSE клиентам
    s.sseClientsMu.Lock()
    defer s.sseClientsMu.Unlock()
    for i, sseClient := range s.sseClients {
        _, err := fmt.Fprintf(sseClient.SSE, "data: %s\n\n", messageBytes)
        if err != nil {
            log.Printf("Error sending update to SSE client: %v", err)
            // Если произошла ошибка отправки, удаляем клиента
            s.sseClients = append(s.sseClients[:i], s.sseClients[i+1:]...)
            continue
        }
        if flusher, ok := sseClient.SSE.(http.Flusher); ok {
            flusher.Flush() // Отправляем данные немедленно
        }
    }
}

func (s *LobbyService) AddSSEClient(client *Client) {
	s.sseClientsMu.Lock()
	defer s.sseClientsMu.Unlock()
	s.sseClients = append(s.sseClients, client)
}
func (s *LobbyService) RemoveSSEClient(client *Client) {
	s.sseClientsMu.Lock()
	defer s.sseClientsMu.Unlock()
	for i, c := range s.sseClients {
			if c == client {
					s.sseClients = append(s.sseClients[:i], s.sseClients[i+1:]...)
					break
			}
	}
}


func (s *LobbyService) AddClientToLobby(lobbyID uuid.UUID, client *Client) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.clients[lobbyID]; !ok {
			s.clients[lobbyID] = make(map[uuid.UUID]*Client)
	}
	s.clients[lobbyID][client.UserID] = client
	return nil
}
func (s *LobbyService) RemoveClientFromLobby(lobbyID, userID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.clients[lobbyID]; ok {
			delete(s.clients[lobbyID], userID)
			if len(s.clients[lobbyID]) == 0 {
					delete(s.clients, lobbyID) 
			}
	}
}

func (s *LobbyService) SendClientMessages(client *Client) {
	for {
			select {
			case message, ok := <-client.Send:
					if !ok {
						
							client.Conn.WriteMessage(websocket.CloseMessage, []byte{})
							return
					}

					
					err := client.Conn.WriteMessage(websocket.TextMessage, message)
					if err != nil {
							log.Println("Write error:", err)
							client.Conn.Close()
							return
					}
			}
	}
}
func (s *LobbyService) BroadcastMessage(lobbyID uuid.UUID, message []byte) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if clients, ok := s.clients[lobbyID]; ok {
			for userID, client := range clients { // Iterate over a copy of the client map
					client.mu.Lock() // Lock the individual client
					err := client.Conn.WriteMessage(websocket.TextMessage, message)
					client.mu.Unlock()

					if err != nil {
							log.Printf("Write error for user %s in lobby %s: %v", userID, lobbyID, err)
							s.disconnectClient(client) // Disconnect client on error
					}
			}
	}
}


func (s *LobbyService) disconnectClient(client *Client) {
        client.Conn.Close()
        close(client.Send)
        s.RemoveClientFromLobby(client.LobbyID, client.UserID)
}

func (s *LobbyService) ListenClientMessages(ctx context.Context, client *Client) { 
    defer func() {
        client.Conn.Close()
        s.RemoveClientFromLobby(client.LobbyID, client.UserID)
        log.Printf("Client %s disconnected from lobby %s", client.UserID, client.LobbyID)
    }()

    for {
		messageType, message, err := client.Conn.ReadMessage()
        if err != nil {
            if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
                log.Printf("Unexpected close error: %v", err)
            }
            break
        }
		if messageType == websocket.TextMessage {
            var msg struct {
                Type string `json:"type"`
            }
            if err := json.Unmarshal(message, &msg); err == nil {
                switch msg.Type {
                case "ping":
                    err := client.Conn.WriteJSON(map[string]string{"type": "pong"})
                    if err != nil {
                        log.Println("WebSocket pong error:", err)
             
                    }
         
                case "chat":
                    s.BroadcastMessage(client.LobbyID, message)
                default:
                    log.Printf("Unknown message type: %s", msg.Type)
                }
            }
		}
   

 
        s.BroadcastMessage(client.LobbyID, message)
    }
}

func (s *LobbyService) GetLobby(id uuid.UUID) (*models.Lobby, error) {
    return s.lobbyRepo.GetLobby(id)
}
func (s *LobbyService) GetAllLobbies(ctx context.Context) ([]*models.Lobby, error) {
    return s.lobbyRepo.GetAllLobbies(ctx)
}
func (s *LobbyService) CloseLobby(ctx context.Context,id uuid.UUID) error {
    return s.lobbyRepo.UpdateLobbyStatus(ctx,id, models.Closed)
}
func (s *LobbyService) StartGame(ctx context.Context,id uuid.UUID) error {
    return s.lobbyRepo.UpdateLobbyStatus(ctx, id, models.InProgress)
}
func (s *LobbyService) GetOpenLobbies(ctx context.Context) ([]*models.Lobby, error) {
    return s.lobbyRepo.GetOpenLobbies(ctx)
}
func (s *LobbyService) ArchiveGame(ctx context.Context, id uuid.UUID) error {
    return s.lobbyRepo.UpdateLobbyStatus(ctx,id, models.Closed)
}