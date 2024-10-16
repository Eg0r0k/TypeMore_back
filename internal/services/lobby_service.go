package services

import (
	"log"
	"net/http"
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
			return true 
	},
}

type Client struct {
	UserID  uuid.UUID
	Conn    *websocket.Conn
	Send    chan []byte
	LobbyID uuid.UUID
}
type LobbyService struct {
	lobbyRepo *repositories.LobbyRepository
	clients   map[uuid.UUID][]*Client 
}
func NewLobbyService(lobbyRepo *repositories.LobbyRepository) *LobbyService {
	return &LobbyService{
			lobbyRepo: lobbyRepo,
			clients:   make(map[uuid.UUID][]*Client),
	}
}

func (s *LobbyService) CreateLobby(ownerID uuid.UUID, name string, isPublic bool, password string, maxPlayers int) (*models.Lobby, error){
	lobbyID, err := uuid.NewV7()
    if err != nil {
        return nil, err
    }
	newLobby:= &models.Lobby{
		LobbyID:   lobbyID,
		CreateAt: time.Now(),
		UpdatedAt: time.Now(),
		IsPublic: isPublic,
		Status: models.Active,
		OwnerID: ownerID,
		Name: name ,
		IsOpen: true,
		Players: []uuid.UUID{ownerID},
	}
	if !isPublic && password != ""{
		newLobby.Password = utils.HashPassword(string(newLobby.Password))
	}
	err = s.lobbyRepo.CreateLobby(newLobby)
	if err != nil {
		return nil,err
	}
	return newLobby,nil
}



func (s *LobbyService) AddClientToLobby(lobbyID uuid.UUID, client *Client) error {
	_, err := s.lobbyRepo.GetLobby(lobbyID)
	if err != nil {
			return err
	}
	s.clients[lobbyID] = append(s.clients[lobbyID], client)
	return nil
}
func (s *LobbyService) ListenClientMessages(client Client) {
    defer func() {
        client.Conn.Close()
        s.removeClient(client.LobbyID, client)
    }()

    for {
        _, message, err := client.Conn.ReadMessage()
        if err != nil {
            log.Printf("error: %v", err)
            break
        }

        log.Printf("Received message from user %s in lobby %s: %s", client.UserID, client.LobbyID, message)
        s.BroadcastMessage(client.LobbyID, message)
    }
}
func (s *LobbyService) removeClient(lobbyID uuid.UUID, client Client) {
    clients := s.clients[lobbyID]
    for i, c := range clients {
        if c == &client {
            s.clients[lobbyID] = append(clients[:i], clients[i+1:]...)
            break
        }
    }
}

func (s *LobbyService) SendClientMessages(client Client) {
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
    clients := s.clients[lobbyID]
    for _, client := range clients {
        select {
        case client.Send <- message:
          
        default:

            log.Printf("Client %s in lobby %s is not receiving messages", client.UserID, client.LobbyID)

            s.disconnectClient(client)
        }
    }
}
func (s *LobbyService) disconnectClient(client *Client) {
    client.Conn.Close()
    close(client.Send)
    s.removeClient(client.LobbyID, *client)
}

func (s *LobbyService) GetLobby(id uuid.UUID) (*models.Lobby, error) {
    return s.lobbyRepo.GetLobby(id)
}
func (s *LobbyService) GetAllLobbies() ([]*models.Lobby, error) {
    return s.lobbyRepo.GetAllLobbies()
}
func (s *LobbyService) CloseLobby(id uuid.UUID) error {
    return s.lobbyRepo.UpdateLobbyStatus(id, models.Closed)
}
func (s *LobbyService) StartGame(id uuid.UUID) error {
    return s.lobbyRepo.UpdateLobbyStatus(id, models.InProgress)
}

func (s *LobbyService) ArchiveGame(id uuid.UUID) error {
    return s.lobbyRepo.UpdateLobbyStatus(id, models.Closed)
}