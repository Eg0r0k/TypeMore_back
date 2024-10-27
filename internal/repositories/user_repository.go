package repositories

import (
	"context"
	"database/sql"
	"fmt"
	"time"
	"typeMore/internal/models"

	"github.com/google/uuid"
)

type UserRepository struct {
	db *sql.DB
}

func NewUserRepository(db *sql.DB) *UserRepository {
	return &UserRepository{db: db}
}
func (r *UserRepository) DeleteUser(ctx context.Context, id uuid.UUID) error {
    _, err := r.db.ExecContext(ctx, "DELETE FROM users WHERE id = $1", id)
    return err
}
func (r *UserRepository) GetUserByID(ctx context.Context, id uuid.UUID) (*models.User, error) {
    return r.getUser(ctx, "WHERE id = $1", id)
}


func (r *UserRepository) GetUserByUsername(ctx context.Context, username string) (*models.User, error) {
    return r.getUser(ctx, "WHERE username = $1", username)
}
func (r *UserRepository ) GetUserByEmail(ctx context.Context, email string) (*models.User, error){
    return r.getUser(ctx, "WHERE email = $1", email)
}

func (r *UserRepository) DeleteRefreshToken(ctx context.Context, userID uuid.UUID, token string) error {
    _, err := r.db.ExecContext(ctx, `DELETE FROM refresh_tokens WHERE user_id = $1 AND token = $2`, userID, token)
    return err
}

func (r *UserRepository) getUser(ctx context.Context, whereClause string, args ...interface{}) (*models.User, error) {
    u := &models.User{}
    var lastIn, lastOut, registrationDate sql.NullTime

    err := r.db.QueryRowContext(ctx, fmt.Sprintf(`
            SELECT id, username, email, is_banned, config, password, 
                       created_at, updated_at, last_in, last_out, registration_date 
            FROM users 
            %s`, whereClause), args...).Scan(
            &u.UserId, &u.Username, &u.Email, &u.IsBanned, &u.Config, &u.Password,
            &u.CreatedAt, &u.UpdatedAt, &lastIn, &lastOut, &registrationDate,
    )
    if err != nil {
            if err == sql.ErrNoRows {
                    return nil, nil
            }
            return nil, fmt.Errorf("querying user: %w", err)
    }

    u.LastIn = convertNullTime(lastIn)
    u.LastOut = convertNullTime(lastOut)
    u.RegistrationDate = convertNullTime(registrationDate)

    u.Roles, err = r.getUserRoles(ctx, u.UserId)
    if err != nil {
            return nil, fmt.Errorf("querying user roles: %w", err)
    }

    return u, nil
}
func (r *UserRepository) getUserRoles(ctx context.Context, userID uuid.UUID) ([]models.Role, error) {
    rows, err := r.db.QueryContext(ctx, `
            SELECT r.name 
            FROM user_roles ur 
            JOIN roles r ON ur.role_id = r.id 
            WHERE ur.user_id = $1`, userID)
    if err != nil {
            return nil, fmt.Errorf("querying user roles: %w", err)
    }
    defer rows.Close()

    var roles []models.Role
    for rows.Next() {
            var roleName string
            if err := rows.Scan(&roleName); err != nil {
                    return nil, fmt.Errorf("scanning role: %w", err)
            }
            role := models.RoleFromString(roleName)
            if role != models.InvalidRole {
                    roles = append(roles, role)
            }
    }

    return roles, nil
}

func convertNullTime(nullTime sql.NullTime) *time.Time {
    if nullTime.Valid {
            return &nullTime.Time
    }
    return nil
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

func (r *UserRepository) CreateRefreshToken(ctx context.Context, token *models.RefreshToken) error {
    _, err := r.db.ExecContext(ctx, `
            INSERT INTO refresh_tokens (id, user_id, token, expires_at, created_at)
            VALUES ($1, $2, $3, $4, $5)`,
            token.ID, token.UserID, token.Token, token.ExpiresAt, token.CreatedAt)
    return err
}

func (r *UserRepository) IsUsernameTaken(ctx context.Context, username string) (bool, error) {
    var exists bool
    err := r.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE username = $1)", username).Scan(&exists)
    return exists, err
}

func (r *UserRepository) IsEmailTaken(ctx context.Context, email string) (bool, error) {
    var exists bool
    err := r.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE email = $1)", email).Scan(&exists)
    return exists, err
}
func (r *UserRepository) UpsertOAuthAccount(ctx context.Context, account *models.OAuthAccount) error {
    tx, err := r.db.BeginTx(ctx, nil)
    if err != nil {
        return fmt.Errorf("beginning transaction: %w", err)
    }
    defer tx.Rollback()

    result, err := tx.ExecContext(ctx, `
        UPDATE oauth_accounts 
        SET email = $1, 
            name = $2, 
            access_token = $3, 
            refresh_token = $4, 
            expires_at = $5,
            updated_at = CURRENT_TIMESTAMP
        WHERE provider = $6 AND provider_user_id = $7
        RETURNING id`,
        account.Email,
        account.Name,
        account.AccessToken,
        account.RefreshToken,
        account.ExpiresAt,
        account.Provider,
        account.ProviderUserID)
    
    if err != nil {
        return fmt.Errorf("updating oauth account: %w", err)
    }

    rowsAffected, err := result.RowsAffected()
    if err != nil {
        return fmt.Errorf("checking rows affected: %w", err)
    }

    if rowsAffected == 0 {
        account.ID = uuid.New()
        account.CreatedAt = time.Now()
        account.UpdatedAt = account.CreatedAt

        _, err = tx.ExecContext(ctx, `
            INSERT INTO oauth_accounts (
                id, user_id, provider, provider_user_id, 
                email, name, access_token, refresh_token, 
                expires_at, created_at, updated_at
            ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
            account.ID,
            account.UserID,
            account.Provider,
            account.ProviderUserID,
            account.Email,
            account.Name,
            account.AccessToken,
            account.RefreshToken,
            account.ExpiresAt,
            account.CreatedAt,
            account.UpdatedAt)

        if err != nil {
            return fmt.Errorf("inserting oauth account: %w", err)
        }
    }

    return tx.Commit()
}
func (r *UserRepository) GetOAuthAccount(ctx context.Context, provider, providerUserID string) (*models.OAuthAccount, error) {
    account := &models.OAuthAccount{}
    err := r.db.QueryRowContext(ctx, `
        SELECT id, user_id, provider, provider_user_id, 
               email, name, access_token, refresh_token, 
               expires_at, created_at, updated_at
        FROM oauth_accounts 
        WHERE provider = $1 AND provider_user_id = $2`,
        provider, providerUserID).Scan(
        &account.ID,
        &account.UserID,
        &account.Provider,
        &account.ProviderUserID,
        &account.Email,
        &account.Name,
        &account.AccessToken,
        &account.RefreshToken,
        &account.ExpiresAt,
        &account.CreatedAt,
        &account.UpdatedAt)

    if err == sql.ErrNoRows {
        return nil, nil
    }
    if err != nil {
        return nil, fmt.Errorf("querying oauth account: %w", err)
    }

    return account, nil
}


//! Возможно переписать
func (r *UserRepository) AddUserRole(ctx context.Context, userID uuid.UUID, role string) error {
    tx, err := r.db.BeginTx(ctx, nil)
    if err != nil {
        return fmt.Errorf("begin transaction: %w", err)
    }
    defer tx.Rollback()

    _, err = tx.ExecContext(ctx, `
        INSERT INTO user_roles (user_id, role_id)
        SELECT $1, r.id FROM roles r WHERE r.name = $2`, userID, role)
    if err != nil {
        return fmt.Errorf("add user role: %w", err)
    }

    return tx.Commit()
}

func (r *UserRepository) RemoveUserRole(ctx context.Context, userID uuid.UUID, role string) error {
    tx, err := r.db.BeginTx(ctx, nil)
    if err != nil {
        return fmt.Errorf("begin transaction: %w", err)
    }
    defer tx.Rollback()

    _, err = tx.ExecContext(ctx, `
        DELETE FROM user_roles 
        WHERE user_id = $1 AND role_id = (SELECT id FROM roles WHERE name = $2)`, userID, role)
    if err != nil {
        return fmt.Errorf("remove user role: %w", err)
    }

    return tx.Commit()
}