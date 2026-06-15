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

// Player is a resolved player record. Tag is the public id; SteamID stays server-side.
type Player struct {
	SteamID  string
	Username string
	Tag      string
}

// UpsertPlayer creates or updates the player keyed by steam_id. An empty
// username leaves any existing name untouched. Returns the resolved player.
func (s *Store) UpsertPlayer(ctx context.Context, steamID, username string) (Player, error) {
	var u *string
	if username != "" {
		u = &username
	}
	var name *string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO players (steam_id, username)
		VALUES ($1, $2)
		ON CONFLICT (steam_id)
		DO UPDATE SET username = COALESCE(EXCLUDED.username, players.username), updated_at = NOW()
		RETURNING username
	`, steamID, u).Scan(&name)
	if err != nil {
		return Player{}, err
	}
	return playerOf(steamID, name), nil
}

// GetPlayer looks up a player by steam_id; ok is false when none exists.
func (s *Store) GetPlayer(ctx context.Context, steamID string) (p Player, ok bool, err error) {
	var name *string
	err = s.pool.QueryRow(ctx, `SELECT username FROM players WHERE steam_id=$1`, steamID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Player{}, false, nil
	}
	if err != nil {
		return Player{}, false, err
	}
	return playerOf(steamID, name), true, nil
}

// LeaderboardEntry is one row of the hourly board (public fields only).
type LeaderboardEntry struct {
	Tag      string `json:"tag"`
	Username string `json:"username"`
	Points   int    `json:"points"`
}

// HourlyLeaderboard returns up to limit players for the current UTC clock-hour,
// highest points first.
func (s *Store) HourlyLeaderboard(ctx context.Context, limit int) ([]LeaderboardEntry, error) {
	bucket := time.Now().UTC().Truncate(time.Hour)
	rows, err := s.pool.Query(ctx, `
		SELECT hs.steam_id, p.username, hs.points
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
		var name *string
		var pts int
		if err := rows.Scan(&steamID, &name, &pts); err != nil {
			return nil, err
		}
		uname := deref(name)
		out = append(out, LeaderboardEntry{Tag: session.PlayerTag(steamID, uname), Username: uname, Points: pts})
	}
	return out, rows.Err()
}

func playerOf(steamID string, name *string) Player {
	uname := deref(name)
	return Player{SteamID: steamID, Username: uname, Tag: session.PlayerTag(steamID, uname)}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
