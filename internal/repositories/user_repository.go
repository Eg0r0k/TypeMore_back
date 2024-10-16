package repositories

import (
	"database/sql"
	"fmt"
	"typeMore/internal/models"

	"github.com/google/uuid"
)

type UserRepository struct {
	db *sql.DB
}

func NewUserRepository(db *sql.DB) *UserRepository {
	return &UserRepository{db: db}
}
func (r *UserRepository) DeleteUser(id uuid.UUID) error {
    _, err := r.db.Exec("DELETE FROM users WHERE id = $1", id)
    return err
}
func (r *UserRepository) GetUserByUsername(username string) (*models.User, error) {
    u := &models.User{}
    var lastIn, lastOut, registrationDate sql.NullTime

    err := r.db.QueryRow(`
        SELECT id, username, email, is_banned, config, password, 
               created_at, updated_at, last_in, last_out, registration_date 
        FROM users 
        WHERE username = $1`, username).Scan(
        &u.UserId, &u.Username, &u.Email, &u.IsBanned, &u.Config, &u.Password,
        &u.CreatedAt, &u.UpdatedAt, &lastIn, &lastOut, &registrationDate,
    )
    if err != nil {
        if err == sql.ErrNoRows {
            return nil, nil
        }
        return nil, fmt.Errorf("error querying user: %w", err)
    }

    if lastIn.Valid {
        u.LastIn = &lastIn.Time
    }
    if lastOut.Valid {
        u.LastOut = &lastOut.Time
    }
    if registrationDate.Valid {
        u.RegistrationDate = &registrationDate.Time
    }

    rows, err := r.db.Query(`
        SELECT r.name 
        FROM user_roles ur 
        JOIN roles r ON ur.role_id = r.id 
        WHERE ur.user_id = $1`, u.UserId)
    if err != nil {
        return nil, fmt.Errorf("error querying user roles: %w", err)
    }
    defer rows.Close()

    var roles []models.Role
    for rows.Next() {
        var roleName string
        if err := rows.Scan(&roleName); err != nil {
            return nil, fmt.Errorf("error scanning role: %w", err)
        }
        role := models.RoleFromString(roleName)
        if role != models.InvalidRole {
            roles = append(roles, role)
        }
    }

    u.Roles = roles

    return u, nil
}


func (r *UserRepository) GetUserByID(id uuid.UUID) (*models.User, error) {
    u := &models.User{}
    
    var lastIn, lastOut, registrationDate sql.NullTime

    err := r.db.QueryRow(`
        SELECT id, username, email, is_banned, config, password, 
               created_at, updated_at, last_in, last_out, registration_date 
        FROM users 
        WHERE id = $1`, id).Scan(
        &u.UserId, &u.Username, &u.Email, &u.IsBanned, &u.Config, &u.Password,
        &u.CreatedAt, &u.UpdatedAt, &lastIn, &lastOut, &registrationDate,
    )
    if err != nil {
        if err == sql.ErrNoRows {
            return nil, nil
        }
        return nil, fmt.Errorf("error querying user: %w", err)
    }
    if lastIn.Valid {
        u.LastIn = &lastIn.Time
    }
    if lastOut.Valid {
        u.LastOut = &lastOut.Time
    }
    if registrationDate.Valid {
        u.RegistrationDate = &registrationDate.Time
    }

    rows, err := r.db.Query(`
        SELECT r.name 
        FROM user_roles ur 
        JOIN roles r ON ur.role_id = r.id 
        WHERE ur.user_id = $1`, id)
    if err != nil {
        return nil, fmt.Errorf("error querying user roles: %w", err)
    }
    defer rows.Close()

    var roles []models.Role
    for rows.Next() {
        var roleName string
        err := rows.Scan(&roleName)
        if err != nil {
            return nil, fmt.Errorf("error scanning role: %w", err)
        }
        role := models.RoleFromString(roleName)
        if role != models.Role(-1) {
            roles = append(roles, role)
        }
    }
    u.Roles = roles

    return u, nil
}


func (r *UserRepository) CreateUser(u *models.User) error {
	tx, err := r.db.Begin()
    if err != nil {
        return err
    }
    defer tx.Rollback()
	_, err = tx.Exec(`
	INSERT INTO users (id, username, email, password, is_banned, config, 
					   created_at, updated_at, last_in, last_out, registration_date)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		u.UserId, u.Username, u.Email, u.Password, u.IsBanned, u.Config,
		u.CreatedAt, u.UpdatedAt, u.LastIn, u.LastOut, u.RegistrationDate)
	if err != nil {
		return err
	}
	for _, role := range u.Roles {
		_, err = tx.Exec("INSERT INTO user_roles (user_id, role_id) VALUES ($1, $2)",
			u.UserId, role)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *UserRepository) GetAccessTokenByToken(token string) (*models.AccessToken, error) {
    at := &models.AccessToken{}
    err := r.db.QueryRow(`
        SELECT id, user_id, token, expires_at, created_at
        FROM access_tokens
        WHERE token = $1`, token).Scan(
        &at.ID, &at.UserID, &at.Token, &at.ExpiresAt, &at.CreatedAt)
    if err != nil {
        if err == sql.ErrNoRows {
            return nil, nil
        }
        return nil, err
    }
    return at, nil
}

func (r *UserRepository) CreateRefreshToken(token *models.RefreshToken) error {
    _, err := r.db.Exec(`
        INSERT INTO refresh_tokens (id, user_id, token, expires_at, created_at)
        VALUES ($1, $2, $3, $4, $5)`,
        token.ID, token.UserID, token.Token, token.ExpiresAt, token.CreatedAt)
    return err
}
func (r *UserRepository) IsUsernameTaken(username string) (bool, error){
	var exists bool 
    err := r.db.QueryRow("SELECT EXISTS(SELECT 1 FROM users WHERE username = $1)", username).Scan(&exists)
	return exists,err
}

func (r *UserRepository) IsEmailTaken(email string) (bool, error){
	var exists bool 
    err := r.db.QueryRow("SELECT EXISTS(SELECT 1 FROM users WHERE email = $1)", email).Scan(&exists)
	return exists, err
}