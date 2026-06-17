package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// AdminStats are the top-level counts shown on the admin dashboard.
type AdminStats struct {
	Players int
	Games   int
	Rounds  int
	Clicks  int
}

// AdminStats returns row counts across the history tables in one round-trip.
func (s *Store) AdminStats(ctx context.Context) (AdminStats, error) {
	var a AdminStats
	err := s.pool.QueryRow(ctx, `
		SELECT (SELECT COUNT(*) FROM players),
		       (SELECT COUNT(*) FROM games),
		       (SELECT COUNT(*) FROM game_rounds),
		       (SELECT COUNT(*) FROM round_scores)
	`).Scan(&a.Players, &a.Games, &a.Rounds, &a.Clicks)
	return a, err
}

// AdminGame is one row of the recent-games list: the game plus a few derived
// aggregates (distinct scorers, total scoring clicks) and its winner.
type AdminGame struct {
	ID         string
	StartedAt  time.Time
	EndedAt    time.Time
	Rounds     int
	Scorers    int
	Clicks     int
	WinnerName string // "" when the game had no scoring clicks
	WinnerID   string
}

// RecentGames returns the most recently ended games, newest first, each with
// its scorer/click counts and placement-1 winner.
func (s *Store) RecentGames(ctx context.Context, limit int) ([]AdminGame, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT g.id, g.started_at, g.ended_at, g.rounds,
		       COALESCE(c.scorers, 0), COALESCE(c.clicks, 0),
		       w.steam_id, w.username, w.display_name
		FROM games g
		LEFT JOIN LATERAL (
			SELECT COUNT(DISTINCT rs.steam_id) AS scorers, COUNT(*) AS clicks
			FROM game_rounds r JOIN round_scores rs ON rs.round_id = r.id
			WHERE r.game_id = g.id
		) c ON true
		LEFT JOIN LATERAL (
			SELECT gs.steam_id, p.username, p.display_name
			FROM game_standings gs LEFT JOIN players p ON p.steam_id = gs.steam_id
			WHERE gs.game_id = g.id AND gs.placement = 1
			ORDER BY gs.steam_id LIMIT 1
		) w ON true
		ORDER BY g.ended_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AdminGame{}
	for rows.Next() {
		var g AdminGame
		var wid, name, disp *string
		if err := rows.Scan(&g.ID, &g.StartedAt, &g.EndedAt, &g.Rounds,
			&g.Scorers, &g.Clicks, &wid, &name, &disp); err != nil {
			return nil, err
		}
		g.WinnerID = deref(wid)
		g.WinnerName = pickName(name, disp)
		out = append(out, g)
	}
	return out, rows.Err()
}

// AdminGameDetail is a single game with its rounds and the scoring clicks of
// each round (slot order, player, arrival offset from the arm).
type AdminGameDetail struct {
	ID        string
	StartedAt time.Time
	EndedAt   time.Time
	Rounds    int
	RoundList []AdminRound
}

// AdminRound is one round in the detail view, with its scoring clicks in slot order.
type AdminRound struct {
	RoundNo int
	N       int
	Players int
	ArmedAt time.Time
	Clicks  []AdminClick
}

// AdminClick is one scoring click: slot ("click N"), who took it, and the
// wire-arrival latency in ms from the arm.
type AdminClick struct {
	SlotNo   int
	Name     string
	SteamID  string
	OffsetMs int
}

// GameDetail loads one game with every round and scoring click, ordered by
// round then slot. ok is false when no game has that id.
func (s *Store) GameDetail(ctx context.Context, gameID string) (d AdminGameDetail, ok bool, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT id, started_at, ended_at, rounds FROM games WHERE id = $1`, gameID,
	).Scan(&d.ID, &d.StartedAt, &d.EndedAt, &d.Rounds)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminGameDetail{}, false, nil
	}
	if err != nil {
		return AdminGameDetail{}, false, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT r.round_no, r.n, r.players, r.armed_at,
		       rs.slot_no, rs.steam_id, p.username, p.display_name, rs.offset_ms
		FROM game_rounds r
		LEFT JOIN round_scores rs ON rs.round_id = r.id
		LEFT JOIN players p ON p.steam_id = rs.steam_id
		WHERE r.game_id = $1
		ORDER BY r.round_no, rs.slot_no
	`, gameID)
	if err != nil {
		return AdminGameDetail{}, false, err
	}
	defer rows.Close()

	// Rows arrive ordered by round; fold consecutive rows into per-round buckets.
	// A round with no scoring clicks yields one row with NULL slot/steam (LEFT JOIN).
	var cur *AdminRound
	for rows.Next() {
		var roundNo, n, players int
		var armedAt time.Time
		var slot, offset *int
		var sid, name, disp *string
		if err := rows.Scan(&roundNo, &n, &players, &armedAt, &slot, &sid, &name, &disp, &offset); err != nil {
			return AdminGameDetail{}, false, err
		}
		if cur == nil || cur.RoundNo != roundNo {
			d.RoundList = append(d.RoundList, AdminRound{RoundNo: roundNo, N: n, Players: players, ArmedAt: armedAt})
			cur = &d.RoundList[len(d.RoundList)-1]
		}
		if slot != nil { // a real scoring click (not the empty-round placeholder row)
			cur.Clicks = append(cur.Clicks, AdminClick{
				SlotNo: *slot, Name: pickName(name, disp), SteamID: deref(sid), OffsetMs: derefInt(offset),
			})
		}
	}
	return d, true, rows.Err()
}

// FastestClicker is one row of the fastest-clickers board: a player's mean
// per-round click delta in ms (gap from their previous click that arm; their
// first click of a round measured from the arm) and how many clicks qualified.
type FastestClicker struct {
	SteamID    string
	Name       string
	Clicks     int
	AvgDeltaMs float64
}

// FastestClickers reads the materialized fastest_clickers board, lowest average
// delta (fastest) first, joining player names. Cheap — it reads the precomputed
// matview, refreshed on a timer by RefreshFastestClickers.
func (s *Store) FastestClickers(ctx context.Context, limit int) ([]FastestClicker, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT fc.steam_id, p.username, p.display_name, fc.clicks, fc.avg_delta_ms
		FROM fastest_clickers fc
		LEFT JOIN players p ON p.steam_id = fc.steam_id
		ORDER BY fc.avg_delta_ms ASC, fc.steam_id ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []FastestClicker{}
	for rows.Next() {
		var f FastestClicker
		var name, disp *string
		if err := rows.Scan(&f.SteamID, &name, &disp, &f.Clicks, &f.AvgDeltaMs); err != nil {
			return nil, err
		}
		f.Name = pickName(name, disp)
		out = append(out, f)
	}
	return out, rows.Err()
}

// RefreshFastestClickers recomputes the fastest_clickers materialized view. Run
// on a timer (~10 min). CONCURRENTLY (enabled by the view's unique index) so it
// never blocks concurrent admin reads; it cannot run inside a transaction.
func (s *Store) RefreshFastestClickers(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `REFRESH MATERIALIZED VIEW CONCURRENTLY fastest_clickers`)
	return err
}

// pickName mirrors Player.Name(): the claimed username if set, else the Steam
// display name, else "" (anonymous).
func pickName(name, disp *string) string {
	if n := deref(name); n != "" {
		return n
	}
	return deref(disp)
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
