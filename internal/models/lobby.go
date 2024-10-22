package models

import (
	"time"

	"github.com/google/uuid"
)



type Lobby struct {
	LobbyID 	uuid.UUID 	`json:"id" db:"id"`
	CreateAt 	time.Time	`json:"created_at" db:"created_at"`
	UpdatedAt 	time.Time	`json:"updated_at" db:"updated_at"` 
	IsPublic 	bool		`json:"is_public" db:"is_public"`
	Password 	[]byte		`json:"-" db:"password"`
	Status 		Status 		`json:"status"`
 	OwnerID     uuid.UUID    `json:"owner_id" db:"owner_id"`
	MaxPlayers 	int			`json:"max_players" db:"max_players"`
	Name		string 		`json:"name" db:"name"`
	IsOpen  	 bool   `json:"is_open" db:"is_open"`
    Players     []uuid.UUID  `json:"players" db:"players"` 
	//? GameMode    string       `json:"game_mode" db:"game_mode"`    maybe 
}


type LobbyUpdateType string

const (
    LobbyCreated LobbyUpdateType = "lobby_created"
    LobbyUpdated LobbyUpdateType = "lobby_updated"
    LobbyDeleted LobbyUpdateType = "lobby_deleted" 
)

type LobbyUpdateMessage struct {
    Type  LobbyUpdateType `json:"type"`
    Lobby *Lobby          `json:"lobby"`
}