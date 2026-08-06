package moderation

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/typemore/typemore-server/internal/badges"
	"github.com/typemore/typemore-server/internal/moderation/moderationdb"
)

// Badge grants, the operator half (00029). The player half — arranging a
// showcase out of what they hold — is the profile domain's.
//
// WHY THIS IS A MODERATION SURFACE AND NOT A PROFILE ONE. A grant is something
// done TO an account by somebody with authority, recorded with who did it, and
// undoable by another operator. That is the shape every other thing in this
// package has, and it is why the grant lands behind the same permission gate,
// the same 404-to-everyone-else subtree, and the same audit columns as a ban.

// ErrUnknownBadge is returned when a grant names a code this build does not
// know (internal/badges). It is a refusal rather than an insert because the
// schema's CHECK only bounds the code's SHAPE — this is the only thing between
// a typo'd grant and a row that renders as a blank chip on a public page.
var ErrUnknownBadge = badges.ErrUnknown

// BadgeGrant is one grant as the admin surface serves it.
type BadgeGrant struct {
	Code      string
	GrantedAt time.Time
	// GrantedByName / RevokedByName are display names, nil when the acting
	// account is gone (the columns are ON DELETE SET NULL — a decision outlives
	// the admin who made it) or when there was no account behind the act.
	GrantedByName *string
	RevokedAt     *time.Time
	RevokedByName *string
	// Order is where the badge's OWNER put it in their showcase, nil when they
	// hold it without showing it. Present here so an operator can see that a
	// grant landed but is not on display, rather than wondering why.
	Order *int32
}

// Granted reports whether this grant is live right now.
func (g BadgeGrant) Granted() bool { return g.RevokedAt == nil }

// BadgeHolder is one account holding a badge, for the "who has X" listing.
type BadgeHolder struct {
	UserID      uuid.UUID
	DisplayName string
	GrantedAt   time.Time
}

// GrantBadge gives an account a badge. IDEMPOTENT: granting one the account
// already holds returns the existing grant unchanged — including its
// display_order, so a re-grant never rearranges a showcase its owner arranged.
//
// A fresh grant starts HIDDEN (display_order NULL). A badge appearing on
// somebody's public page has to be their own act; an operator decides that a
// badge is deserved, not that it is displayed.
func (s *Store) GrantBadge(ctx context.Context, userID uuid.UUID, code string, by *uuid.UUID) (BadgeGrant, error) {
	if !badges.Known(code) {
		return BadgeGrant{}, ErrUnknownBadge
	}
	row, err := s.q.GrantBadge(ctx, moderationdb.GrantBadgeParams{
		UserID: userID, BadgeCode: code, GrantedBy: by,
	})
	if err != nil {
		return BadgeGrant{}, err
	}
	return BadgeGrant{Code: row.BadgeCode, GrantedAt: row.GrantedAt, Order: row.DisplayOrder}, nil
}

// GrantBadgeBySystem is the grant with no account behind it — the entry point
// a future achievements pipeline calls when a badge is EARNED rather than
// awarded. Same idempotence and the same hidden-until-showcased rule as an
// operator's grant; granted_by stays NULL, which the surfaces already render
// as an act without an actor. Kept as its own named method so the achievements
// code reads as what it does and never has to know the audit parameter exists.
func (s *Store) GrantBadgeBySystem(ctx context.Context, userID uuid.UUID, code string) (BadgeGrant, error) {
	return s.GrantBadge(ctx, userID, code, nil)
}

// RevokeBadge soft-revokes the live grant of one badge and reports whether
// there was one to revoke.
//
// Idempotent in the same shape an unban is: the predicate matches only a live
// grant, so running it twice revokes once and the caller learns which happened
// from the boolean, never from an error. A revoked badge leaves every showcase
// immediately — the public read's predicate is the revocation itself, not the
// display_order the owner set.
func (s *Store) RevokeBadge(ctx context.Context, userID uuid.UUID, code string, by *uuid.UUID) (bool, error) {
	// Deliberately NOT gated on badges.Known: a code retired from the registry
	// must stay revocable, or a badge could be taken out of circulation and
	// left un-takeable from the accounts holding it.
	_, err := s.q.RevokeBadge(ctx, moderationdb.RevokeBadgeParams{
		UserID: userID, BadgeCode: code, RevokedBy: by,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// BadgesOfUser lists one account's grants, live and revoked, newest first.
// Revocations are included because "why did they used to have that" is the
// question the soft revoke exists to answer.
func (s *Store) BadgesOfUser(ctx context.Context, userID uuid.UUID) ([]BadgeGrant, error) {
	rows, err := s.q.ListBadgesOfUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]BadgeGrant, len(rows))
	for i, r := range rows {
		out[i] = BadgeGrant{
			Code:          r.BadgeCode,
			GrantedAt:     r.GrantedAt,
			GrantedByName: r.GrantedByName,
			RevokedAt:     r.RevokedAt,
			RevokedByName: r.RevokedByName,
			Order:         r.DisplayOrder,
		}
	}
	return out, nil
}

// HoldersOfBadge lists the accounts currently holding a badge.
func (s *Store) HoldersOfBadge(ctx context.Context, code string, limit int32) ([]BadgeHolder, error) {
	rows, err := s.q.ListHoldersOfBadge(ctx, moderationdb.ListHoldersOfBadgeParams{
		BadgeCode: code, Lim: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]BadgeHolder, len(rows))
	for i, r := range rows {
		out[i] = BadgeHolder{UserID: r.UserID, DisplayName: r.DisplayName, GrantedAt: r.GrantedAt}
	}
	return out, nil
}
