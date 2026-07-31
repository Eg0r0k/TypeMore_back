package auth

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Provider identifies how an identity authenticates. These strings match the
// CHECK constraint on auth_identities.provider.
const (
	ProviderEmail  = "email"
	ProviderGitHub = "github"
	ProviderGoogle = "google"
)

// Token purposes, matching the CHECK constraint on email_tokens.purpose.
const (
	PurposeVerify = "verify"
	PurposeReset  = "reset"
)

// --- Domain types ---
//
// These are the auth domain's own types. The PostgreSQL adapter converts sqlc
// rows into these so nothing outside the adapter depends on the generated code.

// User is a TypeMore account.
type User struct {
	ID          uuid.UUID
	DisplayName string
	CreatedAt   time.Time
	// ProfilePublic gates the public /users/{name} surface; open by default.
	ProfilePublic bool
	// KeyboardPublic is the keyboard portrait's own opt-in, independent of
	// ProfilePublic and off by default — per-key timing is effectively
	// biometric (docs/PROFILE.md, "Privacy").
	KeyboardPublic bool
}

// SettingsParams is the input to UpdateUserSettings: the full pair, resolved
// by the handler from the partial PATCH body against the current row.
type SettingsParams struct {
	ProfilePublic  bool
	KeyboardPublic bool
}

// Identity is one way to sign in to a User. For ProviderEmail, Subject is the
// (normalized) email address.
type Identity struct {
	ID            uuid.UUID
	UserID        uuid.UUID
	Provider      string
	Subject       string
	Email         string
	EmailVerified bool
	CreatedAt     time.Time
}

// Session is an active login.
type Session struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

// Credential is the password material for an email account.
type Credential struct {
	UserID    uuid.UUID
	Hash      string
	UpdatedAt time.Time
}

// EmailToken is a single-use verification/reset token (hash never leaves the DB;
// this value type carries only metadata).
type EmailToken struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Purpose   string
	ExpiresAt time.Time
	UsedAt    *time.Time
}

// --- Parameter structs for composite writes ---

// EmailAccountParams is the input to CreateEmailAccount.
type EmailAccountParams struct {
	DisplayName  string
	Email        string // normalized (lower-cased, trimmed)
	PasswordHash string
}

// OAuthAccountParams is the input to CreateOAuthAccount.
type OAuthAccountParams struct {
	DisplayName   string
	Provider      string
	Subject       string
	Email         string // may be empty if the provider withheld it
	EmailVerified bool
}

// IdentityParams describes an identity to attach to an existing user (linking).
type IdentityParams struct {
	Provider      string
	Subject       string
	Email         string
	EmailVerified bool
}

// EmailTokenParams is the input to CreateEmailToken.
type EmailTokenParams struct {
	UserID    uuid.UUID
	Purpose   string
	TokenHash []byte
	ExpiresAt time.Time
}

// Store is the persistence contract for users, identities, credentials, and
// email tokens. It is declared here (the consumer) and implemented by the
// PostgreSQL adapter. Methods that must be atomic (account creation) encapsulate
// their transaction inside the implementation. A missing row is reported as
// ErrNotFound.
type Store interface {
	// CreateEmailAccount atomically creates a user, an unverified email
	// identity, and its credential row.
	CreateEmailAccount(ctx context.Context, p EmailAccountParams) (User, Identity, error)
	// CreateOAuthAccount atomically creates a user and an OAuth identity.
	CreateOAuthAccount(ctx context.Context, p OAuthAccountParams) (User, Identity, error)
	// LinkIdentity attaches a new identity to an existing user.
	LinkIdentity(ctx context.Context, userID uuid.UUID, p IdentityParams) (Identity, error)

	// IdentityByProviderSubject looks up an identity by its (provider, subject).
	IdentityByProviderSubject(ctx context.Context, provider, subject string) (Identity, error)
	// EmailIdentity looks up the email-provider identity for an address.
	EmailIdentity(ctx context.Context, email string) (Identity, error)
	// VerifiedIdentityByEmail returns any verified identity owning the email
	// (used for the no-auto-link collision check).
	VerifiedIdentityByEmail(ctx context.Context, email string) (Identity, error)
	// IdentitiesByUser lists all of a user's sign-in identities (used by the
	// add-email / set-password flows to inspect what the account already has).
	IdentitiesByUser(ctx context.Context, userID uuid.UUID) ([]Identity, error)
	// MarkEmailVerified marks a user's email-provider identity as verified.
	MarkEmailVerified(ctx context.Context, userID uuid.UUID) error

	// User fetches a user by id.
	User(ctx context.Context, id uuid.UUID) (User, error)
	// UpdateUserSettings replaces the account's privacy switches and returns
	// the updated row.
	UpdateUserSettings(ctx context.Context, userID uuid.UUID, p SettingsParams) (User, error)

	// Credential / UpdateCredential read and replace password material.
	Credential(ctx context.Context, userID uuid.UUID) (Credential, error)
	UpdateCredential(ctx context.Context, userID uuid.UUID, hash string) error

	// CreateEmailToken stores a new token; DeleteUserTokens invalidates prior
	// tokens of a purpose (issued before a new one); UseEmailToken atomically
	// consumes a valid token.
	CreateEmailToken(ctx context.Context, p EmailTokenParams) error
	DeleteUserTokens(ctx context.Context, userID uuid.UUID, purpose string) error
	UseEmailToken(ctx context.Context, tokenHash []byte, purpose string) (EmailToken, error)
}

// SessionStore is the session persistence contract, deliberately separate from
// Store so it can be swapped to Redis independently (see the package doc's
// deviation note). A missing/expired session is reported as ErrNotFound.
type SessionStore interface {
	CreateSession(ctx context.Context, tokenHash []byte, userID uuid.UUID, expiresAt time.Time) (Session, error)
	SessionByTokenHash(ctx context.Context, tokenHash []byte) (Session, error)
	TouchSession(ctx context.Context, id uuid.UUID, expiresAt time.Time) error
	DeleteSession(ctx context.Context, tokenHash []byte) error
	DeleteUserSessions(ctx context.Context, userID uuid.UUID) error
}
