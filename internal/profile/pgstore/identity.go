package pgstore

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/typemore/typemore-server/internal/profile"
	"github.com/typemore/typemore-server/internal/profile/profiledb"
)

// The profile's identity half (00029): the self-described fields, the badge
// showcase and the social links.

// Identity returns the account's bio/keyboard. A missing row cannot happen —
// the caller resolved the user first — but it is mapped rather than propagated
// so a race with account deletion reads as "nothing set" instead of a 500.
func (s *Store) Identity(ctx context.Context, userID uuid.UUID) (profile.Identity, error) {
	row, err := s.q.GetPublicProfileIdentity(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return profile.Identity{}, nil
		}
		return profile.Identity{}, err
	}
	return profile.Identity{Bio: row.Bio, Keyboard: row.Keyboard}, nil
}

// ShowcaseBadges returns the displayed badges in their owner's order.
func (s *Store) ShowcaseBadges(ctx context.Context, userID uuid.UUID) ([]profile.ShowcaseBadge, error) {
	rows, err := s.q.ListShowcaseBadges(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]profile.ShowcaseBadge, len(rows))
	for i, r := range rows {
		// display_order is NOT NULL by the query's own predicate; the generated
		// pointer is an artefact of the column being nullable in general.
		var order int32
		if r.DisplayOrder != nil {
			order = *r.DisplayOrder
		}
		out[i] = profile.ShowcaseBadge{Code: r.BadgeCode, Order: order}
	}
	return out, nil
}

// GrantedBadges returns every live grant, shown or not.
func (s *Store) GrantedBadges(ctx context.Context, userID uuid.UUID) ([]profile.GrantedBadge, error) {
	rows, err := s.q.ListGrantedBadges(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]profile.GrantedBadge, len(rows))
	for i, r := range rows {
		out[i] = profile.GrantedBadge{Code: r.BadgeCode, GrantedAt: r.GrantedAt, Order: r.DisplayOrder}
	}
	return out, nil
}

// Links returns the account's social links, ordered by kind.
func (s *Store) Links(ctx context.Context, userID uuid.UUID) ([]profile.Link, error) {
	rows, err := s.q.ListUserLinks(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]profile.Link, len(rows))
	for i, r := range rows {
		out[i] = profile.Link{Kind: r.Kind, Handle: r.Handle}
	}
	return out, nil
}

// ApplyProfilePatch writes one owner edit inside a single transaction.
//
// ATOMIC BECAUSE THE EDIT IS ONE GESTURE. The settings screen saves a bio, a
// set of links and a badge arrangement together; committing them separately
// would let a failure land a profile that is half the old one and half the new,
// which its owner cannot see and did not ask for.
//
// The showcase is written as CLEAR-then-PLACE rather than as a diff. A diff
// would have to decide what an absent code means, and the answer — "hide it" —
// is exactly what clearing already does, in one statement, with no chance of
// two codes ending up on the same position because a delete was missed.
func (s *Store) ApplyProfilePatch(ctx context.Context, userID uuid.UUID, patch profile.ProfilePatch) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)

	if patch.SetBio || patch.SetKeyboard {
		// The UPDATE writes BOTH columns, so an unmentioned one has to be
		// resolved against what is stored — otherwise editing a bio would erase
		// the keyboard. Read inside the transaction, not in the handler: read
		// and write are then one atomic act, and two saves racing cannot
		// interleave into a row neither of them asked for.
		bio, keyboard := patch.Bio, patch.Keyboard
		if !patch.SetBio || !patch.SetKeyboard {
			current, err := q.GetPublicProfileIdentity(ctx, userID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if !patch.SetBio {
				bio = current.Bio
			}
			if !patch.SetKeyboard {
				keyboard = current.Keyboard
			}
		}
		if err := q.UpdateProfileIdentity(ctx, profiledb.UpdateProfileIdentityParams{
			UserID: userID, Bio: bio, Keyboard: keyboard,
		}); err != nil {
			return err
		}
	}

	for kind, handle := range patch.Links {
		if handle == "" {
			if err := q.DeleteUserLink(ctx, profiledb.DeleteUserLinkParams{
				UserID: userID, Kind: kind,
			}); err != nil {
				return err
			}
			continue
		}
		if err := q.UpsertUserLink(ctx, profiledb.UpsertUserLinkParams{
			UserID: userID, Kind: kind, Handle: handle,
		}); err != nil {
			return err
		}
	}

	if patch.Showcase != nil {
		if err := q.ClearBadgeShowcase(ctx, userID); err != nil {
			return err
		}
		for i, code := range patch.Showcase {
			// The UPDATE's own `revoked_at IS NULL` predicate is what makes a
			// revocation racing this save a no-op rather than a badge being put
			// back on a public page. The handler's earlier validation cannot
			// cover that window; this does.
			order := int32(i)
			if err := q.PlaceBadgeInShowcase(ctx, profiledb.PlaceBadgeInShowcaseParams{
				UserID: userID, BadgeCode: code, DisplayOrder: &order,
			}); err != nil {
				return err
			}
		}
	}

	return tx.Commit(ctx)
}
