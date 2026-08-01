// Package moderation issues, revokes and reads account bans.
//
// Scope is deliberately narrow and stated here so it is not widened by
// accident: a ban stops a player's runs being COUNTED and hides their
// leaderboard entries. It does not touch login, sessions, rooms or matches. A
// banned player can still sign in and still play a casual match against a
// friend, and TestABannedUserStillLogsInAndPlays holds that.
//
// "Banned right now" is defined once, in the `active_bans` SQL view, and every
// reader in the server is behind it — the leaderboard views, both public replay
// reads, the run-submission gate and GET /me. Expiry is therefore evaluated at
// read time, which is why a temporary ban needs no janitor: it lifts itself the
// moment it lapses, and an unban restores a player's board entries instantly
// with no rebuild, because the entries were never deleted.
//
// See docs/MODERATION.md.
package moderation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/typemore/typemore-server/internal/moderation/moderationdb"
)

// ErrNoSuchUser is returned when an identifier resolves to no account.
var ErrNoSuchUser = errors.New("moderation: no such user")

// ErrAmbiguousUser is returned when an identifier resolves to more than one
// account. It carries the candidates so the caller can print them: picking one
// on the operator's behalf is how the wrong person gets banned.
type ErrAmbiguousUser struct {
	Identifier string
	Candidates []User
}

func (e *ErrAmbiguousUser) Error() string {
	return fmt.Sprintf("moderation: %q matches %d accounts", e.Identifier, len(e.Candidates))
}

// User is an account, as much of one as moderation needs.
type User struct {
	ID          uuid.UUID
	DisplayName string
}

// Ban is one ban record.
type Ban struct {
	ID     uuid.UUID
	UserID uuid.UUID
	// DisplayName is filled in by the listing reads, not by Ban/Unban.
	DisplayName string
	// Reason is an INTERNAL moderation note. It is never rendered to the
	// banned player and no endpoint returns it.
	Reason    string
	IssuedBy  string
	IssuedAt  time.Time
	ExpiresAt *time.Time
	RevokedAt *time.Time
	// UserRestricted is whether the USER is restricted right now, which is not
	// the same as whether THIS ban is in force: a listing shows revoked and
	// lapsed rows too.
	UserRestricted bool
}

// Active reports whether this particular ban is in force, evaluated in Go for
// display only. The authority is the active_bans view; this exists so a listing
// can mark a row without a round trip per row.
func (b Ban) Active(now time.Time) bool {
	return b.RevokedAt == nil && (b.ExpiresAt == nil || b.ExpiresAt.After(now))
}

// Store is moderation's database surface.
type Store struct {
	pool *pgxpool.Pool
	q    *moderationdb.Queries
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: moderationdb.New(pool)}
}

// IsRestricted is the only question the rest of the server asks. It answers
// yes or no and nothing else: no reason, no expiry, so there is nothing here
// for a handler to leak into a response.
func (s *Store) IsRestricted(ctx context.Context, userID uuid.UUID) (bool, error) {
	return s.q.IsRestricted(ctx, userID)
}

// ResolveUser turns what a moderator has in hand — a uuid, a display name, or
// an email — into exactly one account, or an error that says why not.
//
// The order is uuid, then email, then display name, and it is not arbitrary:
// the first two are unique by construction and the third is not, so trying the
// unambiguous forms first means an operator who pasted an id never risks
// matching somebody's chosen nickname.
func (s *Store) ResolveUser(ctx context.Context, identifier string) (User, error) {
	if id, err := uuid.Parse(identifier); err == nil {
		row, err := s.q.ResolveUserByID(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, fmt.Errorf("%w: %s", ErrNoSuchUser, identifier)
		}
		if err != nil {
			return User{}, err
		}
		return User{ID: row.ID, DisplayName: row.DisplayName}, nil
	}

	for _, lookup := range []func(context.Context, string) ([]User, error){
		s.byEmail, s.byDisplayName,
	} {
		found, err := lookup(ctx, identifier)
		if err != nil {
			return User{}, err
		}
		switch len(found) {
		case 0:
			continue
		case 1:
			return found[0], nil
		default:
			return User{}, &ErrAmbiguousUser{Identifier: identifier, Candidates: found}
		}
	}
	return User{}, fmt.Errorf("%w: %s", ErrNoSuchUser, identifier)
}

func (s *Store) byEmail(ctx context.Context, email string) ([]User, error) {
	rows, err := s.q.ResolveUserByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	out := make([]User, len(rows))
	for i := range rows {
		out[i] = User{ID: rows[i].ID, DisplayName: rows[i].DisplayName}
	}
	return out, nil
}

func (s *Store) byDisplayName(ctx context.Context, name string) ([]User, error) {
	rows, err := s.q.ResolveUserByDisplayName(ctx, name)
	if err != nil {
		return nil, err
	}
	out := make([]User, len(rows))
	for i := range rows {
		out[i] = User{ID: rows[i].ID, DisplayName: rows[i].DisplayName}
	}
	return out, nil
}

// Actor is who performs a moderation act: the account id for the audit
// column (00023) and the display name for the human-readable issued_by note.
// Filled from the authenticated admin by the HTTP surface; moderation itself
// never resolves a principal — the reader is supplied from outside, like
// every other cross-domain fact. A zero ID records NULL in the audit column:
// an act with a note but no account behind it (tests, one-off tooling).
type Actor struct {
	ID   uuid.UUID
	Name string
}

// auditID is the FK-safe form of the actor for the issued_by_user /
// revoked_by_user columns.
func (a Actor) auditID() *uuid.UUID {
	if a.ID == uuid.Nil {
		return nil
	}
	return &a.ID
}

// BanResult reports what Ban actually did, so the caller can render the change
// rather than a success message that is true either way.
type BanResult struct {
	Ban     Ban
	Amended bool
	// Previous is the ban as it stood before an amendment, so the caller can
	// show what moved.
	Previous *Ban
}

// Ban puts a user under restriction, or amends the restriction they are already
// under.
//
// Idempotent by design: running it twice is not an error and does not stack a
// second ban. Two simultaneous bans on one account would make "when does this
// lift" a question with two answers, and a moderator correcting an expiry
// should not have to unban first.
func (s *Store) Ban(ctx context.Context, userID uuid.UUID, reason string, by Actor, expiresAt *time.Time) (BanResult, error) {
	existing, err := s.q.ActiveBanFor(ctx, userID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		row, err := s.q.InsertBan(ctx, moderationdb.InsertBanParams{
			UserID: userID, Reason: reason, IssuedBy: &by.Name, IssuedByUser: by.auditID(),
			ExpiresAt: expiresAt,
		})
		if err != nil {
			return BanResult{}, err
		}
		return BanResult{Ban: banOf(row.ID, row.UserID, row.Reason, row.IssuedBy, row.IssuedAt, row.ExpiresAt, row.RevokedAt)}, nil
	case err != nil:
		return BanResult{}, err
	}

	before := banOf(existing.ID, existing.UserID, existing.Reason, existing.IssuedBy,
		existing.IssuedAt, existing.ExpiresAt, existing.RevokedAt)
	row, err := s.q.UpdateBan(ctx, moderationdb.UpdateBanParams{
		ID: existing.ID, Reason: reason, IssuedBy: &by.Name, IssuedByUser: by.auditID(),
		ExpiresAt: expiresAt,
	})
	if err != nil {
		return BanResult{}, err
	}
	return BanResult{
		Ban:      banOf(row.ID, row.UserID, row.Reason, row.IssuedBy, row.IssuedAt, row.ExpiresAt, row.RevokedAt),
		Amended:  true,
		Previous: &before,
	}, nil
}

// ErrNotBanned is returned by Unban when the user is not under restriction.
var ErrNotBanned = errors.New("moderation: user is not banned")

// Unban revokes the active ban. Revocation is a fact with a time, not a
// deletion: the row stays so "what happened to this account" has an answer.
func (s *Store) Unban(ctx context.Context, userID uuid.UUID, by Actor) (Ban, error) {
	existing, err := s.q.ActiveBanFor(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ban{}, ErrNotBanned
	}
	if err != nil {
		return Ban{}, err
	}
	row, err := s.q.RevokeBan(ctx, moderationdb.RevokeBanParams{
		ID: existing.ID, RevokedByUser: by.auditID(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Ban{}, ErrNotBanned
	}
	if err != nil {
		return Ban{}, err
	}
	return banOf(row.ID, row.UserID, row.Reason, row.IssuedBy, row.IssuedAt, row.ExpiresAt, row.RevokedAt), nil
}

// List returns bans newest first; onlyActive filters to those in force.
func (s *Store) List(ctx context.Context, onlyActive bool, limit int32) ([]Ban, error) {
	rows, err := s.q.ListBans(ctx, moderationdb.ListBansParams{OnlyActive: onlyActive, RowLimit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]Ban, len(rows))
	for i := range rows {
		b := banOf(rows[i].ID, rows[i].UserID, rows[i].Reason, rows[i].IssuedBy,
			rows[i].IssuedAt, rows[i].ExpiresAt, rows[i].RevokedAt)
		b.DisplayName = rows[i].DisplayName
		b.UserRestricted = rows[i].UserRestricted
		out[i] = b
	}
	return out, nil
}

// History returns every ban a user has ever had, newest first.
func (s *Store) History(ctx context.Context, userID uuid.UUID) ([]Ban, error) {
	rows, err := s.q.ListBansForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]Ban, len(rows))
	for i := range rows {
		b := banOf(rows[i].ID, rows[i].UserID, rows[i].Reason, rows[i].IssuedBy,
			rows[i].IssuedAt, rows[i].ExpiresAt, rows[i].RevokedAt)
		b.DisplayName = rows[i].DisplayName
		b.UserRestricted = rows[i].UserRestricted
		out[i] = b
	}
	return out, nil
}

func banOf(id, userID uuid.UUID, reason string, issuedBy *string, issuedAt time.Time, expiresAt, revokedAt *time.Time) Ban {
	by := ""
	if issuedBy != nil {
		by = *issuedBy
	}
	return Ban{
		ID: id, UserID: userID, Reason: reason, IssuedBy: by,
		IssuedAt: issuedAt, ExpiresAt: expiresAt, RevokedAt: revokedAt,
	}
}
