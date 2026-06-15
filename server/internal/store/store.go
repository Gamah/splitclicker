// Package store is the Postgres-backed persistence: the hourly leaderboard and
// the players table. It implements game.Store (the engine's scoring sink) and
// serves leaderboard/player reads for the API.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/gamah/splitclicker/internal/game"
	"github.com/gamah/splitclicker/internal/session"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// compile-time check that Store satisfies the engine's persistence interface.
var _ game.Store = (*Store)(nil)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// AddHourlyPoints upserts each delta into the player's (steam_id, hour_bucket)
// tally for the given UTC clock-hour. Implements game.Store.
func (s *Store) AddHourlyPoints(ctx context.Context, bucket time.Time, deltas []game.HourlyDelta) error {
	batch := &pgx.Batch{}
	for _, d := range deltas {
		batch.Queue(`
			INSERT INTO hourly_scores (steam_id, hour_bucket, points)
			VALUES ($1, $2, $3)
			ON CONFLICT (steam_id, hour_bucket)
			DO UPDATE SET points = hourly_scores.points + EXCLUDED.points
		`, d.SteamID, bucket, d.Points)
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

// Player is a resolved player record. Tag is the public id; SteamID stays
// server-side. Username is the claimed handle (may be ""); DisplayName is the
// Steam name reported by the client (may be "").
type Player struct {
	SteamID     string
	Username    string
	DisplayName string
	Tag         string
}

// Name is the public display string: the claimed username, falling back to the
// Steam display name. Empty only when the player has neither.
func (p Player) Name() string {
	if p.Username != "" {
		return p.Username
	}
	return p.DisplayName
}

// UpsertPlayer creates or updates the player keyed by steam_id. An empty
// username or displayName leaves any existing value untouched. Returns the
// resolved player.
func (s *Store) UpsertPlayer(ctx context.Context, steamID, username, displayName string) (Player, error) {
	var u, d *string
	if username != "" {
		u = &username
	}
	if displayName != "" {
		d = &displayName
	}
	var name, disp *string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO players (steam_id, username, display_name)
		VALUES ($1, $2, $3)
		ON CONFLICT (steam_id)
		DO UPDATE SET username = COALESCE(EXCLUDED.username, players.username),
		             display_name = COALESCE(EXCLUDED.display_name, players.display_name),
		             updated_at = NOW()
		RETURNING username, display_name
	`, steamID, u, d).Scan(&name, &disp)
	if err != nil {
		return Player{}, err
	}
	return playerOf(steamID, name, disp), nil
}

// GetPlayer looks up a player by steam_id; ok is false when none exists.
func (s *Store) GetPlayer(ctx context.Context, steamID string) (p Player, ok bool, err error) {
	var name, disp *string
	err = s.pool.QueryRow(ctx, `SELECT username, display_name FROM players WHERE steam_id=$1`, steamID).Scan(&name, &disp)
	if errors.Is(err, pgx.ErrNoRows) {
		return Player{}, false, nil
	}
	if err != nil {
		return Player{}, false, err
	}
	return playerOf(steamID, name, disp), true, nil
}

// LeaderboardEntry is one row of a board. SteamID64 is public (the Steam-profile
// identifier), sent so the client can open/copy a player's community profile.
type LeaderboardEntry struct {
	Tag      string `json:"tag"`
	Username string `json:"username"`
	Points   int    `json:"points"`
	SteamID  string `json:"steam_id"`
}

// HourlyLeaderboard returns up to limit players for the current UTC clock-hour,
// highest points first.
func (s *Store) HourlyLeaderboard(ctx context.Context, limit int) ([]LeaderboardEntry, error) {
	bucket := time.Now().UTC().Truncate(time.Hour)
	rows, err := s.pool.Query(ctx, `
		SELECT hs.steam_id, p.username, p.display_name, hs.points
		FROM hourly_scores hs
		LEFT JOIN players p ON p.steam_id = hs.steam_id
		WHERE hs.hour_bucket = $1
		ORDER BY hs.points DESC, hs.steam_id ASC
		LIMIT $2
	`, bucket, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []LeaderboardEntry{}
	for rows.Next() {
		var steamID string
		var name, disp *string
		var pts int
		if err := rows.Scan(&steamID, &name, &disp, &pts); err != nil {
			return nil, err
		}
		p := playerOf(steamID, name, disp)
		out = append(out, LeaderboardEntry{Tag: p.Tag, Username: p.Name(), Points: pts, SteamID: steamID})
	}
	return out, rows.Err()
}

// HoursWonLeaderboard returns up to limit players by career hours won, highest
// first. Points carries the hours count (reusing the entry shape).
func (s *Store) HoursWonLeaderboard(ctx context.Context, limit int) ([]LeaderboardEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT w.steam_id, p.username, p.display_name, w.hours
		FROM hourly_wins w
		LEFT JOIN players p ON p.steam_id = w.steam_id
		ORDER BY w.hours DESC, w.steam_id ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []LeaderboardEntry{}
	for rows.Next() {
		var steamID string
		var name, disp *string
		var hours int
		if err := rows.Scan(&steamID, &name, &disp, &hours); err != nil {
			return nil, err
		}
		p := playerOf(steamID, name, disp)
		out = append(out, LeaderboardEntry{Tag: p.Tag, Username: p.Name(), Points: hours, SteamID: steamID})
	}
	return out, rows.Err()
}

// AddSessionWin credits one game win to steamID on the persistent sessions-won
// board. Implements game.Store; called when a game's final standings are settled.
func (s *Store) AddSessionWin(ctx context.Context, steamID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO session_wins (steam_id, wins) VALUES ($1, 1)
		ON CONFLICT (steam_id) DO UPDATE SET wins = session_wins.wins + 1
	`, steamID)
	return err
}

// SessionsWonLeaderboard returns up to limit players by career sessions (games)
// won, highest first. Points carries the wins count (reusing the entry shape).
func (s *Store) SessionsWonLeaderboard(ctx context.Context, limit int) ([]LeaderboardEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sw.steam_id, p.username, p.display_name, sw.wins
		FROM session_wins sw
		LEFT JOIN players p ON p.steam_id = sw.steam_id
		ORDER BY sw.wins DESC, sw.steam_id ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []LeaderboardEntry{}
	for rows.Next() {
		var steamID string
		var name, disp *string
		var wins int
		if err := rows.Scan(&steamID, &name, &disp, &wins); err != nil {
			return nil, err
		}
		p := playerOf(steamID, name, disp)
		out = append(out, LeaderboardEntry{Tag: p.Tag, Username: p.Name(), Points: wins, SteamID: steamID})
	}
	return out, rows.Err()
}

// FinalizeDueHours credits an hourly win to the top scorer of every completed
// clock-hour (strictly before the current UTC hour) not yet finalized. It is
// idempotent — the finalized_hours ledger guards against double-counting across
// restarts — so it is safe to call on startup and on every hour boundary.
// Returns the number of hours newly finalized.
func (s *Store) FinalizeDueHours(ctx context.Context, now time.Time) (int, error) {
	current := now.UTC().Truncate(time.Hour)
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT hs.hour_bucket
		FROM hourly_scores hs
		WHERE hs.hour_bucket < $1
		  AND NOT EXISTS (SELECT 1 FROM finalized_hours f WHERE f.hour_bucket = hs.hour_bucket)
		ORDER BY hs.hour_bucket
	`, current)
	if err != nil {
		return 0, err
	}
	var buckets []time.Time
	for rows.Next() {
		var b time.Time
		if err := rows.Scan(&b); err != nil {
			rows.Close()
			return 0, err
		}
		buckets = append(buckets, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	n := 0
	for _, b := range buckets {
		if err := s.finalizeHour(ctx, b); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// finalizeHour credits the winner of one completed hour in a single transaction:
// pick the top scorer (most points, steam_id asc tiebreak), bump their hours_won,
// and stamp the ledger. The ledger insert is the idempotency point.
func (s *Store) finalizeHour(ctx context.Context, bucket time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var winner string
	err = tx.QueryRow(ctx, `
		SELECT steam_id FROM hourly_scores
		WHERE hour_bucket = $1
		ORDER BY points DESC, steam_id ASC
		LIMIT 1
	`, bucket).Scan(&winner)
	if errors.Is(err, pgx.ErrNoRows) {
		// No scores for the hour: nothing to award, but still mark it done.
		if _, err := tx.Exec(ctx, `INSERT INTO finalized_hours (hour_bucket) VALUES ($1) ON CONFLICT DO NOTHING`, bucket); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO hourly_wins (steam_id, hours) VALUES ($1, 1)
		ON CONFLICT (steam_id) DO UPDATE SET hours = hourly_wins.hours + 1
	`, winner); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO finalized_hours (hour_bucket) VALUES ($1) ON CONFLICT DO NOTHING`, bucket); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// playerOf resolves a player. The tag is keyed on the claimed username only (so
// it stays stable as the Steam display name changes); Name() handles the
// public-display fallback to the Steam name.
func playerOf(steamID string, name, disp *string) Player {
	uname := deref(name)
	return Player{SteamID: steamID, Username: uname, DisplayName: deref(disp), Tag: session.PlayerTag(steamID, uname)}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
