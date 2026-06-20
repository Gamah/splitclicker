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

// The three competitive boards (top clickers, hours won, games won) are scoped
// to the active bounty's window: each is derived from history filtered by the
// window start (the active bounty's activated_at), so a new bounty automatically
// shows fresh/zero boards while the all-time history is preserved untouched.
// "since" is that window start; the zero time means all-time (the fallback used
// when no bounty is active). They are read only by the LeaderboardCache.

// HourlyLeaderboard returns up to limit players by points scored since the window
// start (sumacross every UTC-hour bucket from sinceHour on), highest first.
// sinceHour should be the window start truncated to the hour; with no active
// bounty the cache passes the current hour, reproducing the per-hour board.
func (s *Store) HourlyLeaderboard(ctx context.Context, sinceHour time.Time, limit int) ([]LeaderboardEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT hs.steam_id, p.username, p.display_name, SUM(hs.points)::INT
		FROM hourly_scores hs
		LEFT JOIN players p ON p.steam_id = hs.steam_id
		WHERE hs.hour_bucket >= $1
		GROUP BY hs.steam_id, p.username, p.display_name
		ORDER BY SUM(hs.points) DESC, hs.steam_id ASC
		LIMIT $2
	`, sinceHour.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBoard(rows)
}

// HoursWonLeaderboard returns up to limit players by UTC clock-hours won within
// the window, highest first. A completed hour is "won" by its top scorer (points
// desc, steam_id asc — same rule as the hour finalizer); the derivation reads
// hourly_scores directly so it needs no per-window win table. The in-progress
// hour (>= currentHour) is excluded since it isn't won yet.
func (s *Store) HoursWonLeaderboard(ctx context.Context, since, currentHour time.Time, limit int) ([]LeaderboardEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT w.steam_id, p.username, p.display_name, COUNT(*)::INT
		FROM (
			SELECT DISTINCT ON (hour_bucket) hour_bucket, steam_id
			FROM hourly_scores
			WHERE hour_bucket >= $1 AND hour_bucket < $2
			ORDER BY hour_bucket, points DESC, steam_id ASC
		) w
		LEFT JOIN players p ON p.steam_id = w.steam_id
		GROUP BY w.steam_id, p.username, p.display_name
		ORDER BY COUNT(*) DESC, w.steam_id ASC
		LIMIT $3
	`, since.UTC(), currentHour.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBoard(rows)
}

// AllTimeClickers returns up to limit players by total scoring clicks across all
// history (every round_scores row, all bounties), highest first. This is the one
// board that never resets — the lifetime "top clickers" of the whole DB.
func (s *Store) AllTimeClickers(ctx context.Context, limit int) ([]LeaderboardEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT rs.steam_id, p.username, p.display_name, COUNT(*)::INT
		FROM round_scores rs
		LEFT JOIN players p ON p.steam_id = rs.steam_id
		GROUP BY rs.steam_id, p.username, p.display_name
		ORDER BY COUNT(*) DESC, rs.steam_id ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBoard(rows)
}

// scanBoard reads board rows of the shared shape (steam_id, username,
// display_name, count) into LeaderboardEntry values; count maps to Points.
func scanBoard(rows pgx.Rows) ([]LeaderboardEntry, error) {
	out := []LeaderboardEntry{}
	for rows.Next() {
		var steamID string
		var name, disp *string
		var count int
		if err := rows.Scan(&steamID, &name, &disp, &count); err != nil {
			return nil, err
		}
		p := playerOf(steamID, name, disp)
		out = append(out, LeaderboardEntry{Tag: p.Tag, Username: p.Name(), Points: count, SteamID: steamID})
	}
	return out, rows.Err()
}

// RecordGame writes a completed game's full history — the games row, its
// game_rounds, and one round_scores row per scoring click — in a single
// transaction. Implements game.Store; called once at game end off the hot path.
// The games insert is idempotent (ON CONFLICT DO NOTHING) so a retry after a
// partial failure can't duplicate a game.
func (s *Store) RecordGame(ctx context.Context, log game.GameLog) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `
		INSERT INTO games (id, started_at, ended_at, rounds)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO NOTHING
	`, log.GameID, log.StartedAt, log.EndedAt, log.Rounds)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Game already recorded by an earlier attempt — nothing more to do.
		return tx.Commit(ctx)
	}

	batch := &pgx.Batch{}
	for _, r := range log.RoundLogs {
		batch.Queue(`
			INSERT INTO game_rounds (id, game_id, round_no, n, players, armed_at)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, r.RoundID, log.GameID, r.RoundNo, r.N, r.Players, r.ArmedAt)
		for _, c := range r.Clicks {
			batch.Queue(`
				INSERT INTO round_scores (round_id, slot_no, steam_id, offset_ms)
				VALUES ($1, $2, $3, $4)
			`, r.RoundID, c.SlotNo, c.SteamID, c.OffsetMs)
		}
		// Anticheat checks the round flagged ride the same transaction, so they FK
		// cleanly to the game_rounds row inserted just above.
		for _, ch := range r.Checks {
			batch.Queue(`
				INSERT INTO anticheat_checks (round_id, steam_id, check_type, detail)
				VALUES ($1, $2, $3, $4)
			`, r.RoundID, ch.SteamID, ch.Type, ch.Detail)
		}
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RecordTestSent persists a test the engine issued to a flagged player. The id is
// the engine token the answer must echo; answer/correct stay NULL until answered.
// Implements game.Store.
func (s *Store) RecordTestSent(ctx context.Context, t game.TestRecord) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO anticheat_tests (id, steam_id, test_kind, prompt, expected)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO NOTHING
	`, t.ID, t.SteamID, t.Kind, t.Prompt, t.Expected)
	return err
}

// RecordTestAnswer settles a sent test with the player's answer. A wrong answer
// is recorded too (the engine then issues a fresh test), so the table holds the
// full attempt trail. Implements game.Store.
func (s *Store) RecordTestAnswer(ctx context.Context, id, answer string, correct bool) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE anticheat_tests
		SET answer = $2, correct = $3, answered_at = now()
		WHERE id = $1
	`, id, answer, correct)
	return err
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

// SessionsWonLeaderboard returns up to limit players by games (sessions) won
// within the window — placement-1 finishes in games that ended after the window
// start — highest first. This is the board the bounty winner is read from (the
// window leader); derived from game history so it scopes to the active bounty
// and the cumulative session_wins table stays untouched for all-time records.
func (s *Store) SessionsWonLeaderboard(ctx context.Context, since time.Time, limit int) ([]LeaderboardEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT gs.steam_id, p.username, p.display_name, COUNT(*)::INT
		FROM game_standings gs
		JOIN games g ON g.id = gs.game_id
		LEFT JOIN players p ON p.steam_id = gs.steam_id
		WHERE gs.placement = 1 AND g.ended_at > $1
		GROUP BY gs.steam_id, p.username, p.display_name
		ORDER BY COUNT(*) DESC, gs.steam_id ASC
		LIMIT $2
	`, since.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBoard(rows)
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
