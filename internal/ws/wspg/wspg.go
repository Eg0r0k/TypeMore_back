// Package wspg is the PostgreSQL implementation of the ws domain's MatchStore.
// It writes a finished match and its per-participant capture in one transaction,
// using raw pgx (the shape is a straight insert, no query reuse to justify sqlc).
package wspg

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/typemore/typemore-server/internal/ws"
)

// Store implements ws.MatchStore against Postgres.
type Store struct {
	pool *pgxpool.Pool
}

// Compile-time check that Store satisfies the consumer interface.
var _ ws.MatchStore = (*Store)(nil)

// New builds a Store from a pgx pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// SaveMatch writes the match header and one match_run per participant atomically.
// The jsonb columns take the raw JSON as text (Postgres parses it); log is the
// gzip capture blob (bytea); a guest's empty user id becomes SQL NULL.
func (s *Store) SaveMatch(ctx context.Context, m ws.MatchRecord) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO matches
				(id, room_code, name, settings, freemods, seed, dict_hash, lang, go_at, ended_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			m.ID, m.RoomCode, m.Name, string(m.Settings), string(m.Freemods),
			m.Seed, m.DictHash, m.Lang, m.GoAt, m.EndedAt,
		); err != nil {
			return fmt.Errorf("insert match: %w", err)
		}

		for _, run := range m.Runs {
			var uid *uuid.UUID
			if run.UserID != "" {
				parsed, err := uuid.Parse(run.UserID)
				if err != nil {
					return fmt.Errorf("parse user id %q: %w", run.UserID, err)
				}
				uid = &parsed
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO match_runs
					(match_id, player_id, nick, user_id, freemods, log, batch_count, final_status)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				m.ID, run.PlayerID, run.Nick, uid, string(run.Freemods),
				run.Log, run.BatchCount, run.FinalStatus,
			); err != nil {
				return fmt.Errorf("insert match_run: %w", err)
			}
		}
		return nil
	})
}
