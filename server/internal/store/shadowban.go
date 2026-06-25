package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Shadowban is one silently-banned account. A banned connection still receives
// `hello` + `armed` (so the client looks alive and visibly sees the world arm),
// but every other frame is withheld and all its inbound messages are dropped, so
// it can click forever and never score, win, or appear on a board — with nothing
// that reveals the ban. Managed from the admin pages; enforced at the next WS
// connect (see api.wsConnect and ws.Client.Shadowbanned).
type Shadowban struct {
	SteamID   string
	Reason    string
	CreatedAt time.Time
}

// IsShadowbanned reports whether steamID is on the silent-ban list. Queried once
// per WS connect — a primary-key lookup — so the check stays current without an
// in-memory cache to keep in sync.
func (s *Store) IsShadowbanned(ctx context.Context, steamID string) (bool, error) {
	var one int
	err := s.pool.QueryRow(ctx,
		`SELECT 1 FROM shadowbans WHERE steam_id = $1`, steamID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// AddShadowban puts steamID on the silent-ban list (idempotent — re-banning just
// refreshes the reason). Takes effect on the player's next connect.
func (s *Store) AddShadowban(ctx context.Context, steamID, reason string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO shadowbans (steam_id, reason)
		VALUES ($1, $2)
		ON CONFLICT (steam_id) DO UPDATE SET reason = EXCLUDED.reason
	`, steamID, reason)
	return err
}

// RemoveShadowban lifts steamID's silent ban (a no-op if it wasn't banned). Takes
// effect on the player's next connect.
func (s *Store) RemoveShadowban(ctx context.Context, steamID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM shadowbans WHERE steam_id = $1`, steamID)
	return err
}

// ListShadowbans returns every silently-banned account, newest first, for the
// admin management view.
func (s *Store) ListShadowbans(ctx context.Context) ([]Shadowban, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT steam_id, reason, created_at FROM shadowbans ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Shadowban{}
	for rows.Next() {
		var b Shadowban
		if err := rows.Scan(&b.SteamID, &b.Reason, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
