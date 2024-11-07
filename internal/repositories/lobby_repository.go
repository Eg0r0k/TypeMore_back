package repositories

import (
	"context"
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

func (r *LobbyRepository) CreateLobby(ctx context.Context, l *models.Lobby) error {
    tx, err := r.db.BeginTx(ctx, nil) 
    if err != nil {
            return err
    }
    defer tx.Rollback() 

    _, err = tx.ExecContext(ctx, `
            INSERT INTO lobbies (id, created_at, updated_at, is_public, password, status, owner_id, max_players, name, is_open, players)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
    `, l.LobbyID, l.CreateAt, l.UpdatedAt, l.IsPublic, l.Password, l.Status, l.OwnerID, l.MaxPlayers, l.Name, l.IsOpen, pq.Array(l.Players))
    if err != nil {
            return err
    }

    return tx.Commit() 
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
func (r *LobbyRepository) GetAllLobbies(ctx context.Context) ([]*models.Lobby, error) { 
    rows, err := r.db.QueryContext(ctx, ` 
        SELECT id, created_at, updated_at, is_public, password, status, owner_id, max_players, name, is_open, players
        FROM lobbies  
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
    if err := rows.Err(); err != nil { 
        return nil, err
    }

    return lobbies, nil
}

func (r *LobbyRepository) UpdateLobbyStatus(ctx context.Context, id uuid.UUID, status models.Status) error {
    tx, err := r.db.BeginTx(ctx, nil)
    if err != nil {
            return err
    }
    defer tx.Rollback()

    _, err = tx.ExecContext(ctx, "UPDATE lobbies SET status = $1, updated_at = NOW() WHERE id = $2", status, id)
    if err != nil {
            return err
    }

    return tx.Commit()
}


func (r *LobbyRepository) GetOpenLobbies(ctx context.Context) ([]*models.Lobby, error) {
    rows, err := r.db.QueryContext(ctx, `
            SELECT id, created_at, updated_at, is_public, owner_id, max_players, name, is_open, players 
            FROM lobbies
            WHERE is_open = TRUE AND status = $1 AND is_public = TRUE
    `, models.Active) 
    if err != nil {
            return nil, err
    }
    defer rows.Close()

    var lobbies []*models.Lobby
    for rows.Next() {
            l := &models.Lobby{}
            err := rows.Scan(&l.LobbyID, &l.CreateAt, &l.UpdatedAt, &l.IsPublic, &l.OwnerID, &l.MaxPlayers, &l.Name, &l.IsOpen, pq.Array(&l.Players))
            if err != nil {
                    return nil, err
            }
            lobbies = append(lobbies, l)
    }
    return lobbies, nil
}


func (r *LobbyRepository) UpdateLobby(ctx context.Context, l *models.Lobby) error {
    tx, err := r.db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    _, err = tx.ExecContext(ctx, `
        UPDATE lobbies 
        SET updated_at = $2, is_public = $3, password = $4, status = $5, max_players = $6, name = $7, is_open = $8, players = $9
        WHERE id = $1
    `, l.LobbyID, l.UpdatedAt, l.IsPublic, l.Password, l.Status, l.MaxPlayers, l.Name, l.IsOpen, pq.Array(l.Players))
    if err != nil {
        return err
    }

    return tx.Commit()
}

func (r *LobbyRepository) JoinLobby(ctx context.Context, lobbyID, userID uuid.UUID) error {
    tx, err := r.db.BeginTx(ctx, nil)
    if err != nil {
            return err
    }
    defer tx.Rollback()

    var players []uuid.UUID
    err = tx.QueryRowContext(ctx, "SELECT players FROM lobbies WHERE id = $1", lobbyID).Scan(pq.Array(&players))
    if err != nil {
            return err
    }

    players = append(players, userID)

    _, err = tx.ExecContext(ctx, "UPDATE lobbies SET players = $1 WHERE id = $2", pq.Array(players), lobbyID)
    if err != nil {
            return err
    }

    return tx.Commit()
}


func (r *LobbyRepository) LeaveLobby(ctx context.Context, lobbyID, userID uuid.UUID) error {
    tx, err := r.db.BeginTx(ctx, nil)
    if err != nil {
            return err
    }
    defer tx.Rollback()

    var players []uuid.UUID
    err = tx.QueryRowContext(ctx, "SELECT players FROM lobbies WHERE id = $1", lobbyID).Scan(pq.Array(&players))
    if err != nil {
            return err
    }

    newPlayers := make([]uuid.UUID, 0, len(players)-1)
    for _, p := range players {
            if p != userID {
                    newPlayers = append(newPlayers, p)
            }
    }

    _, err = tx.ExecContext(ctx, "UPDATE lobbies SET players = $1 WHERE id = $2", pq.Array(newPlayers), lobbyID)
    if err != nil {
            return err
    }

    return tx.Commit()
}

func (r *LobbyRepository) DeleteLobby(ctx context.Context, id uuid.UUID) error {
    tx, err := r.db.BeginTx(ctx, nil)
    if err != nil {
            return err
    }
    defer tx.Rollback()

    _, err = tx.ExecContext(ctx, "DELETE FROM lobbies WHERE id = $1", id)
    if err != nil {
            return err
    }

    return tx.Commit()
}

