package repositories

import (
	"database/sql"
	"typeMore/internal/models"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

type LobbyRepository struct {
	db *sql.DB
}

func NewLobbyRepository(db *sql.DB) *LobbyRepository {
	return &LobbyRepository{db:db}
}

func (r *LobbyRepository) CreateLobby(l *models.Lobby) error {
    _, err := r.db.Exec(`
        INSERT INTO lobbies (id, created_at, updated_at, is_public, password, status, owner_id, max_players, name, is_open, players)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
    `, l.LobbyID, l.CreateAt, l.UpdatedAt, l.IsPublic, l.Password, l.Status, l.OwnerID, l.MaxPlayers, l.Name, l.IsOpen, pq.Array(l.Players))
    return err
}

func (r *LobbyRepository) GetLobby(id uuid.UUID) (*models.Lobby, error) {
    l := &models.Lobby{}
    err := r.db.QueryRow(`
        SELECT id, created_at, updated_at, is_public, password, status, owner_id, max_players, name, is_open, players
        FROM lobbies WHERE id = $1
    `, id).Scan(&l.LobbyID, &l.CreateAt, &l.UpdatedAt, &l.IsPublic, &l.Password, &l.Status, &l.OwnerID, &l.MaxPlayers, &l.Name, &l.IsOpen, pq.Array(&l.Players))
    if err != nil {
        return nil, err
    }
    return l, nil
}
func (r *LobbyRepository) GetAllLobbies() ([]*models.Lobby, error) {
	rows,err:= r.db.Query(`
	SELECT id, created_at, updated_at, is_public
	`)
	if err != nil {
        return nil, err
    }
    defer rows.Close()
	var lobbies []*models.Lobby

    for rows.Next() {
        l := &models.Lobby{}
        err := rows.Scan(&l.LobbyID, &l.CreateAt, &l.UpdatedAt, &l.IsPublic, &l.Password, &l.Status, &l.OwnerID, &l.MaxPlayers, &l.Name, &l.IsOpen, pq.Array(&l.Players))
        if err != nil {
            return nil, err
        }
        lobbies = append(lobbies, l)
    }
    return lobbies, nil
}

func (r *LobbyRepository) UpdateLobbyStatus(id uuid.UUID, status models.Status) error {
    _, err := r.db.Exec("UPDATE lobbies SET status = $1, updated_at = NOW() WHERE id = $2", status, id)
    return err
}