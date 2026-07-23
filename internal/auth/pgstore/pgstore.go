// Package pgstore is the PostgreSQL implementation of the auth domain's Store
// and SessionStore interfaces, backed by the sqlc-generated authdb queries. It
// converts generated rows into auth domain types so nothing outside this
// package depends on the generated code, and encapsulates multi-statement
// transactions (account creation) behind single interface methods.
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/typemore/typemore-server/internal/auth"
	"github.com/typemore/typemore-server/internal/auth/authdb"
)

// Store implements auth.Store and auth.SessionStore against Postgres.
type Store struct {
	pool *pgxpool.Pool
	q    *authdb.Queries
}

// Compile-time checks that Store satisfies the consumer interfaces.
var (
	_ auth.Store        = (*Store)(nil)
	_ auth.SessionStore = (*Store)(nil)
	_ auth.Cleaner      = (*Store)(nil)
)

// New builds a Store from a pgx pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: authdb.New(pool)}
}

// mapErr translates pgx's no-rows sentinel and the schema's named constraint
// violations into domain errors, leaving everything else as-is.
func mapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.ConstraintName {
		case "users_display_name_key":
			return auth.ErrDisplayNameTaken
		case "verified_email_one_user":
			return auth.ErrEmailOwnedByOtherUser
		case "auth_identities_provider_provider_subject_key":
			return auth.ErrIdentityExists
		}
	}
	return err
}

// --- conversions ---

func toUser(u authdb.User) auth.User {
	return auth.User{ID: u.ID, DisplayName: u.DisplayName, CreatedAt: u.CreatedAt}
}

func toIdentity(i authdb.AuthIdentity) auth.Identity {
	email := ""
	if i.Email != nil {
		email = *i.Email
	}
	return auth.Identity{
		ID:            i.ID,
		UserID:        i.UserID,
		Provider:      i.Provider,
		Subject:       i.ProviderSubject,
		Email:         email,
		EmailVerified: i.EmailVerified,
		CreatedAt:     i.CreatedAt,
	}
}

func toSession(s authdb.Session) auth.Session {
	return auth.Session{
		ID:         s.ID,
		UserID:     s.UserID,
		CreatedAt:  s.CreatedAt,
		ExpiresAt:  s.ExpiresAt,
		LastSeenAt: s.LastSeenAt,
	}
}

// emailPtr maps "" to a NULL email column and a non-empty value to a pointer.
func emailPtr(email string) *string {
	if email == "" {
		return nil
	}
	return &email
}

// --- Store: account creation (transactional) ---

// CreateEmailAccount creates user + unverified email identity + credential in one
// transaction.
func (s *Store) CreateEmailAccount(ctx context.Context, p auth.EmailAccountParams) (auth.User, auth.Identity, error) {
	var (
		user     auth.User
		identity auth.Identity
	)
	err := s.tx(ctx, func(q *authdb.Queries) error {
		u, err := q.CreateUser(ctx, p.DisplayName)
		if err != nil {
			return fmt.Errorf("create user: %w", mapErr(err))
		}
		id, err := q.CreateIdentity(ctx, authdb.CreateIdentityParams{
			UserID:          u.ID,
			Provider:        auth.ProviderEmail,
			ProviderSubject: p.Email,
			Email:           emailPtr(p.Email),
			EmailVerified:   false,
		})
		if err != nil {
			return fmt.Errorf("create identity: %w", mapErr(err))
		}
		if err := q.UpsertCredential(ctx, authdb.UpsertCredentialParams{
			UserID:       u.ID,
			Argon2idHash: p.PasswordHash,
		}); err != nil {
			return fmt.Errorf("create credential: %w", err)
		}
		user, identity = toUser(u), toIdentity(id)
		return nil
	})
	return user, identity, err
}

// CreateOAuthAccount creates user + OAuth identity in one transaction.
func (s *Store) CreateOAuthAccount(ctx context.Context, p auth.OAuthAccountParams) (auth.User, auth.Identity, error) {
	var (
		user     auth.User
		identity auth.Identity
	)
	err := s.tx(ctx, func(q *authdb.Queries) error {
		u, err := q.CreateUser(ctx, p.DisplayName)
		if err != nil {
			return fmt.Errorf("create user: %w", mapErr(err))
		}
		id, err := q.CreateIdentity(ctx, authdb.CreateIdentityParams{
			UserID:          u.ID,
			Provider:        p.Provider,
			ProviderSubject: p.Subject,
			Email:           emailPtr(p.Email),
			EmailVerified:   p.EmailVerified,
		})
		if err != nil {
			return fmt.Errorf("create identity: %w", mapErr(err))
		}
		user, identity = toUser(u), toIdentity(id)
		return nil
	})
	return user, identity, err
}

// LinkIdentity attaches a new identity to an existing user.
func (s *Store) LinkIdentity(ctx context.Context, userID uuid.UUID, p auth.IdentityParams) (auth.Identity, error) {
	id, err := s.q.CreateIdentity(ctx, authdb.CreateIdentityParams{
		UserID:          userID,
		Provider:        p.Provider,
		ProviderSubject: p.Subject,
		Email:           emailPtr(p.Email),
		EmailVerified:   p.EmailVerified,
	})
	if err != nil {
		return auth.Identity{}, mapErr(err)
	}
	return toIdentity(id), nil
}

// --- Store: lookups & updates ---

func (s *Store) IdentityByProviderSubject(ctx context.Context, provider, subject string) (auth.Identity, error) {
	id, err := s.q.GetIdentityByProviderSubject(ctx, authdb.GetIdentityByProviderSubjectParams{
		Provider:        provider,
		ProviderSubject: subject,
	})
	if err != nil {
		return auth.Identity{}, mapErr(err)
	}
	return toIdentity(id), nil
}

func (s *Store) EmailIdentity(ctx context.Context, email string) (auth.Identity, error) {
	id, err := s.q.GetEmailIdentityByEmail(ctx, emailPtr(email))
	if err != nil {
		return auth.Identity{}, mapErr(err)
	}
	return toIdentity(id), nil
}

func (s *Store) VerifiedIdentityByEmail(ctx context.Context, email string) (auth.Identity, error) {
	id, err := s.q.GetVerifiedIdentityByEmail(ctx, emailPtr(email))
	if err != nil {
		return auth.Identity{}, mapErr(err)
	}
	return toIdentity(id), nil
}

func (s *Store) IdentitiesByUser(ctx context.Context, userID uuid.UUID) ([]auth.Identity, error) {
	rows, err := s.q.ListIdentitiesByUser(ctx, userID)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]auth.Identity, len(rows))
	for i := range rows {
		out[i] = toIdentity(rows[i])
	}
	return out, nil
}

func (s *Store) MarkEmailVerified(ctx context.Context, userID uuid.UUID) error {
	return mapErr(s.q.VerifyEmailIdentityByUser(ctx, userID))
}

func (s *Store) User(ctx context.Context, id uuid.UUID) (auth.User, error) {
	u, err := s.q.GetUserByID(ctx, id)
	if err != nil {
		return auth.User{}, mapErr(err)
	}
	return toUser(u), nil
}

func (s *Store) Credential(ctx context.Context, userID uuid.UUID) (auth.Credential, error) {
	c, err := s.q.GetCredentialByUser(ctx, userID)
	if err != nil {
		return auth.Credential{}, mapErr(err)
	}
	return auth.Credential{UserID: c.UserID, Hash: c.Argon2idHash, UpdatedAt: c.UpdatedAt}, nil
}

func (s *Store) UpdateCredential(ctx context.Context, userID uuid.UUID, hash string) error {
	return mapErr(s.q.UpsertCredential(ctx, authdb.UpsertCredentialParams{UserID: userID, Argon2idHash: hash}))
}

// --- Store: email tokens ---

func (s *Store) CreateEmailToken(ctx context.Context, p auth.EmailTokenParams) error {
	_, err := s.q.CreateEmailToken(ctx, authdb.CreateEmailTokenParams{
		UserID:    p.UserID,
		Purpose:   p.Purpose,
		TokenHash: p.TokenHash,
		ExpiresAt: p.ExpiresAt,
	})
	return mapErr(err)
}

func (s *Store) DeleteUserTokens(ctx context.Context, userID uuid.UUID, purpose string) error {
	return mapErr(s.q.DeleteUserTokensByPurpose(ctx, authdb.DeleteUserTokensByPurposeParams{
		UserID:  userID,
		Purpose: purpose,
	}))
}

// DeleteExpiredSessions removes sessions past their expiry (janitor sweep).
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	n, err := s.q.DeleteExpiredSessions(ctx)
	return n, mapErr(err)
}

// DeleteStaleEmailTokens removes email tokens that expired or were consumed
// more than 24 hours ago (janitor sweep).
func (s *Store) DeleteStaleEmailTokens(ctx context.Context) (int64, error) {
	n, err := s.q.DeleteStaleEmailTokens(ctx)
	return n, mapErr(err)
}

func (s *Store) UseEmailToken(ctx context.Context, tokenHash []byte, purpose string) (auth.EmailToken, error) {
	t, err := s.q.UseEmailToken(ctx, authdb.UseEmailTokenParams{TokenHash: tokenHash, Purpose: purpose})
	if err != nil {
		return auth.EmailToken{}, mapErr(err)
	}
	return auth.EmailToken{
		ID:        t.ID,
		UserID:    t.UserID,
		Purpose:   t.Purpose,
		ExpiresAt: t.ExpiresAt,
		UsedAt:    t.UsedAt,
	}, nil
}

// --- SessionStore ---

func (s *Store) CreateSession(ctx context.Context, tokenHash []byte, userID uuid.UUID, expiresAt time.Time) (auth.Session, error) {
	sess, err := s.q.CreateSession(ctx, authdb.CreateSessionParams{
		TokenHash: tokenHash,
		UserID:    userID,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		return auth.Session{}, mapErr(err)
	}
	return toSession(sess), nil
}

func (s *Store) SessionByTokenHash(ctx context.Context, tokenHash []byte) (auth.Session, error) {
	sess, err := s.q.GetSessionByTokenHash(ctx, tokenHash)
	if err != nil {
		return auth.Session{}, mapErr(err)
	}
	return toSession(sess), nil
}

func (s *Store) TouchSession(ctx context.Context, id uuid.UUID, expiresAt time.Time) error {
	return mapErr(s.q.TouchSession(ctx, authdb.TouchSessionParams{ID: id, ExpiresAt: expiresAt}))
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	return mapErr(s.q.DeleteSessionByTokenHash(ctx, tokenHash))
}

func (s *Store) DeleteUserSessions(ctx context.Context, userID uuid.UUID) error {
	return mapErr(s.q.DeleteUserSessions(ctx, userID))
}

// tx runs fn inside a database transaction, committing on success and rolling
// back on any error (or panic). It is the single place transaction lifecycle is
// handled, so the composite methods above stay readable.
func (s *Store) tx(ctx context.Context, fn func(q *authdb.Queries) error) error {
	pgtx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	// Rollback is a no-op after a successful commit, so this is safe to defer.
	defer func() { _ = pgtx.Rollback(ctx) }()

	if err := fn(s.q.WithTx(pgtx)); err != nil {
		return err
	}
	if err := pgtx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}
