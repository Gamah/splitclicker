package store

import (
	"context"
	"errors"
	"time"

	"github.com/gamah/splitclicker/internal/session"
	"github.com/jackc/pgx/v5"
)

// Window scopes the admin views to a slice of history. When All is set the view
// spans the whole database; otherwise rows are filtered to the half-open span
// (Start, End] (matching how a bounty's window is settled). It's the
// admin-side equivalent of the leaderboard cache's "since" filter, generalised
// to an arbitrary bounty window (previous or current) for the filter toggle.
type Window struct {
	All   bool
	Start time.Time
	End   time.Time
}

// AllWindow is the default, unfiltered view (all history).
func AllWindow() Window { return Window{All: true} }

// bounds returns the concrete (start, end] timestamps to bind into queries. For
// the all-history view it returns a span that contains every row, so a single
// windowed query shape serves both cases without dynamic SQL.
func (w Window) bounds() (start, end time.Time) {
	if w.All {
		return time.Unix(0, 0).UTC(), time.Now().UTC().Add(24 * time.Hour)
	}
	return w.Start.UTC(), w.End.UTC()
}

// AdminStats are the top-level counts shown on the admin dashboard. Players is
// the all-time account count; the rest are scoped to the active Window.
type AdminStats struct {
	Players int
	Games   int
	Rounds  int
	Clicks  int
	Checks  int // anticheat checks flagged
	Tests   int // anticheat tests sent
}

// AdminStats returns the dashboard counts in one round-trip, scoped to w (games
// by ended_at, checks by created_at, tests by sent_at). Players is always the
// full account count — it isn't a per-window figure.
func (s *Store) AdminStats(ctx context.Context, w Window) (AdminStats, error) {
	start, end := w.bounds()
	var a AdminStats
	err := s.pool.QueryRow(ctx, `
		SELECT (SELECT COUNT(*) FROM players),
		       (SELECT COUNT(*) FROM games g
		          WHERE g.ended_at > $1 AND g.ended_at <= $2),
		       (SELECT COUNT(*) FROM game_rounds r JOIN games g ON g.id = r.game_id
		          WHERE g.ended_at > $1 AND g.ended_at <= $2),
		       (SELECT COUNT(*) FROM round_scores rs
		          JOIN game_rounds r ON r.id = rs.round_id
		          JOIN games g ON g.id = r.game_id
		          WHERE g.ended_at > $1 AND g.ended_at <= $2),
		       (SELECT COUNT(*) FROM anticheat_checks ac
		          WHERE ac.created_at > $1 AND ac.created_at <= $2),
		       (SELECT COUNT(*) FROM anticheat_tests t
		          WHERE t.sent_at > $1 AND t.sent_at <= $2)
	`, start, end).Scan(&a.Players, &a.Games, &a.Rounds, &a.Clicks, &a.Checks, &a.Tests)
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

// RecentGames returns one page of ended games, newest first, scoped to w, each
// with its scorer/click counts and placement-1 winner. The total row count
// (across all pages, within the window) is returned for pagination. No-player
// games are filtered out (the engine no longer writes them, and this hides the
// empty games an idle server recorded before that change shipped).
func (s *Store) RecentGames(ctx context.Context, w Window, limit, offset int) ([]AdminGame, int, error) {
	start, end := w.bounds()
	rows, err := s.pool.Query(ctx, `
		SELECT g.id, g.started_at, g.ended_at, g.rounds,
		       COALESCE(c.scorers, 0), COALESCE(c.clicks, 0),
		       w.steam_id, w.username, w.display_name,
		       COUNT(*) OVER()
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
		WHERE g.ended_at > $1 AND g.ended_at <= $2
		  AND EXISTS (
			-- Hide no-player games (an idle server's, written before the engine
			-- paused on empty): keep only games a player was connected for at arm.
			SELECT 1 FROM game_rounds r WHERE r.game_id = g.id AND r.players > 0
		)
		ORDER BY g.ended_at DESC
		LIMIT $3 OFFSET $4
	`, start, end, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []AdminGame{}
	total := 0
	for rows.Next() {
		var g AdminGame
		var wid, name, disp *string
		if err := rows.Scan(&g.ID, &g.StartedAt, &g.EndedAt, &g.Rounds,
			&g.Scorers, &g.Clicks, &wid, &name, &disp, &total); err != nil {
			return nil, 0, err
		}
		g.WinnerID = deref(wid)
		g.WinnerName = pickName(name, disp)
		out = append(out, g)
	}
	return out, total, rows.Err()
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

// AdminRound is one round in the detail view, with its scoring clicks in slot
// order and any anticheat checks the round flagged.
type AdminRound struct {
	RoundNo int
	N       int
	Players int
	ArmedAt time.Time
	Clicks  []AdminClick
	Checks  []AdminCheck
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
	if err := rows.Err(); err != nil {
		return AdminGameDetail{}, false, err
	}

	// Attach the anticheat checks each round flagged (a separate query so the clicks
	// query above stays a clean per-slot join).
	crows, err := s.pool.Query(ctx, `
		SELECT r.round_no, ac.steam_id, p.username, p.display_name, ac.check_type, ac.detail
		FROM game_rounds r
		JOIN anticheat_checks ac ON ac.round_id = r.id
		LEFT JOIN players p ON p.steam_id = ac.steam_id
		WHERE r.game_id = $1
		ORDER BY r.round_no, ac.id
	`, gameID)
	if err != nil {
		return AdminGameDetail{}, false, err
	}
	defer crows.Close()
	byRound := map[int][]AdminCheck{}
	for crows.Next() {
		var roundNo int
		var ch AdminCheck
		var name, disp *string
		if err := crows.Scan(&roundNo, &ch.SteamID, &name, &disp, &ch.Type, &ch.Detail); err != nil {
			return AdminGameDetail{}, false, err
		}
		ch.Name = pickName(name, disp)
		byRound[roundNo] = append(byRound[roundNo], ch)
	}
	if err := crows.Err(); err != nil {
		return AdminGameDetail{}, false, err
	}
	for i := range d.RoundList {
		d.RoundList[i].Checks = byRound[d.RoundList[i].RoundNo]
	}
	return d, true, nil
}

// AdminAntiCheat is one row of the dashboard's anticheat summary: a player and
// their aggregate counts within the window — checks flagged, and tests they've
// passed / failed. It's the top-level roll-up; the per-event detail (individual
// checks and tests) lives on that player's profile view.
type AdminAntiCheat struct {
	SteamID     string
	Name        string
	Checks      int
	TestsPassed int
	TestsFailed int
}

// AntiCheatAggregate returns one page of the per-player anticheat roll-up scoped
// to w: checks flagged (created_at in window) and tests passed/failed (sent_at
// in window, settled by the `correct` flag — pending tests count as neither).
// Players with no anticheat activity in the window are omitted. Ordered by most
// checks, then most failed tests. Returns the total player count for pagination.
func (s *Store) AntiCheatAggregate(ctx context.Context, w Window, limit, offset int) ([]AdminAntiCheat, int, error) {
	start, end := w.bounds()
	rows, err := s.pool.Query(ctx, `
		WITH ck AS (
			SELECT steam_id, COUNT(*) AS checks
			FROM anticheat_checks
			WHERE created_at > $1 AND created_at <= $2
			GROUP BY steam_id
		),
		ts AS (
			SELECT steam_id,
			       COUNT(*) FILTER (WHERE correct IS TRUE)  AS passed,
			       COUNT(*) FILTER (WHERE correct IS FALSE) AS failed
			FROM anticheat_tests
			WHERE sent_at > $1 AND sent_at <= $2
			GROUP BY steam_id
		),
		agg AS (
			SELECT COALESCE(ck.steam_id, ts.steam_id) AS steam_id,
			       COALESCE(ck.checks, 0) AS checks,
			       COALESCE(ts.passed, 0) AS passed,
			       COALESCE(ts.failed, 0) AS failed
			FROM ck FULL OUTER JOIN ts ON ck.steam_id = ts.steam_id
		)
		SELECT a.steam_id, p.username, p.display_name,
		       a.checks, a.passed, a.failed, COUNT(*) OVER()
		FROM agg a LEFT JOIN players p ON p.steam_id = a.steam_id
		ORDER BY a.checks DESC, a.failed DESC, a.steam_id ASC
		LIMIT $3 OFFSET $4
	`, start, end, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []AdminAntiCheat{}
	total := 0
	for rows.Next() {
		var a AdminAntiCheat
		var name, disp *string
		if err := rows.Scan(&a.SteamID, &name, &disp,
			&a.Checks, &a.TestsPassed, &a.TestsFailed, &total); err != nil {
			return nil, 0, err
		}
		a.Name = pickName(name, disp)
		out = append(out, a)
	}
	return out, total, rows.Err()
}

// AdminCheck is one anticheat check flagged against a player. In the per-player
// list it also carries the game/round it belongs to (zero on the game-detail
// view, which already groups by round).
type AdminCheck struct {
	GameID    string
	RoundNo   int
	SteamID   string
	Name      string
	Type      string
	Detail    string
	CreatedAt time.Time
}

// PlayerChecks returns one page of anticheat checks flagged against steamID
// within w, newest first, each with the game/round it came from. Returns the
// total count for pagination.
func (s *Store) PlayerChecks(ctx context.Context, steamID string, w Window, limit, offset int) ([]AdminCheck, int, error) {
	start, end := w.bounds()
	rows, err := s.pool.Query(ctx, `
		SELECT r.game_id, r.round_no, ac.steam_id, p.username, p.display_name,
		       ac.check_type, ac.detail, ac.created_at, COUNT(*) OVER()
		FROM anticheat_checks ac
		JOIN game_rounds r ON r.id = ac.round_id
		LEFT JOIN players p ON p.steam_id = ac.steam_id
		WHERE ac.steam_id = $1 AND ac.created_at > $2 AND ac.created_at <= $3
		ORDER BY ac.id DESC
		LIMIT $4 OFFSET $5
	`, steamID, start, end, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []AdminCheck{}
	total := 0
	for rows.Next() {
		var ch AdminCheck
		var name, disp *string
		if err := rows.Scan(&ch.GameID, &ch.RoundNo, &ch.SteamID, &name, &disp,
			&ch.Type, &ch.Detail, &ch.CreatedAt, &total); err != nil {
			return nil, 0, err
		}
		ch.Name = pickName(name, disp)
		out = append(out, ch)
	}
	return out, total, rows.Err()
}

// AdminTest is one anticheat test sent to a player and its answer (if any), for
// the audit trail on the player's profile.
type AdminTest struct {
	SteamID    string
	Name       string
	Kind       string
	Prompt     string
	Answer     string // "" until answered
	Answered   bool
	Correct    bool
	SentAt     time.Time
	AnsweredAt *time.Time
}

// PlayerTests returns one page of anticheat tests sent to steamID within w,
// newest first, with the answer received (if any). Returns the total count for
// pagination.
func (s *Store) PlayerTests(ctx context.Context, steamID string, w Window, limit, offset int) ([]AdminTest, int, error) {
	start, end := w.bounds()
	rows, err := s.pool.Query(ctx, `
		SELECT t.steam_id, p.username, p.display_name, t.test_kind, t.prompt,
		       t.answer, t.correct, t.sent_at, t.answered_at, COUNT(*) OVER()
		FROM anticheat_tests t
		LEFT JOIN players p ON p.steam_id = t.steam_id
		WHERE t.steam_id = $1 AND t.sent_at > $2 AND t.sent_at <= $3
		ORDER BY t.sent_at DESC
		LIMIT $4 OFFSET $5
	`, steamID, start, end, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []AdminTest{}
	total := 0
	for rows.Next() {
		var tst AdminTest
		var name, disp, answer *string
		var correct *bool
		if err := rows.Scan(&tst.SteamID, &name, &disp, &tst.Kind, &tst.Prompt,
			&answer, &correct, &tst.SentAt, &tst.AnsweredAt, &total); err != nil {
			return nil, 0, err
		}
		tst.Name = pickName(name, disp)
		tst.Answer = deref(answer)
		tst.Answered = tst.AnsweredAt != nil
		tst.Correct = correct != nil && *correct
		out = append(out, tst)
	}
	return out, total, rows.Err()
}

// PlayerProfile is the per-player admin view: identity plus aggregate stats
// scoped to a Window. SteamID is the authoritative id; Tag is the public tag.
type PlayerProfile struct {
	SteamID     string
	Username    string
	DisplayName string
	Tag         string
	CreatedAt   time.Time
	Clicks      int // scoring clicks in window
	GamesWon    int // placement-1 finishes in window
	Points      int // hourly points in window
	Checks      int
	TestsPassed int
	TestsFailed int
}

// Name mirrors Player.Name(): the claimed username, else the Steam display name.
func (p PlayerProfile) Name() string {
	if p.Username != "" {
		return p.Username
	}
	return p.DisplayName
}

// PlayerProfile loads one player's identity and window-scoped aggregate stats.
// ok is false when no player has that steam_id.
func (s *Store) PlayerProfile(ctx context.Context, steamID string, w Window) (PlayerProfile, bool, error) {
	start, end := w.bounds()
	startHour := start.Truncate(time.Hour)
	var p PlayerProfile
	p.SteamID = steamID
	var name, disp *string
	err := s.pool.QueryRow(ctx, `
		SELECT p.username, p.display_name, p.created_at,
		       (SELECT COUNT(*) FROM round_scores rs
		          JOIN game_rounds r ON r.id = rs.round_id
		          JOIN games g ON g.id = r.game_id
		          WHERE rs.steam_id = p.steam_id AND g.ended_at > $2 AND g.ended_at <= $3),
		       (SELECT COUNT(*) FROM game_standings gs
		          JOIN games g ON g.id = gs.game_id
		          WHERE gs.steam_id = p.steam_id AND gs.placement = 1
		            AND g.ended_at > $2 AND g.ended_at <= $3),
		       (SELECT COALESCE(SUM(points), 0)::INT FROM hourly_scores hs
		          WHERE hs.steam_id = p.steam_id AND hs.hour_bucket >= $4 AND hs.hour_bucket <= $3),
		       (SELECT COUNT(*) FROM anticheat_checks ac
		          WHERE ac.steam_id = p.steam_id AND ac.created_at > $2 AND ac.created_at <= $3),
		       (SELECT COUNT(*) FROM anticheat_tests t
		          WHERE t.steam_id = p.steam_id AND t.correct IS TRUE AND t.sent_at > $2 AND t.sent_at <= $3),
		       (SELECT COUNT(*) FROM anticheat_tests t
		          WHERE t.steam_id = p.steam_id AND t.correct IS FALSE AND t.sent_at > $2 AND t.sent_at <= $3)
		FROM players p WHERE p.steam_id = $1
	`, steamID, start, end, startHour).Scan(
		&name, &disp, &p.CreatedAt, &p.Clicks, &p.GamesWon, &p.Points,
		&p.Checks, &p.TestsPassed, &p.TestsFailed)
	if errors.Is(err, pgx.ErrNoRows) {
		return PlayerProfile{}, false, nil
	}
	if err != nil {
		return PlayerProfile{}, false, err
	}
	p.Username = deref(name)
	p.DisplayName = deref(disp)
	p.Tag = session.PlayerTag(steamID, p.Username)
	return p, true, nil
}

// PlayerSanction is a player's live anticheat ladder state for the ACTIVE bounty:
// the persisted flag count and the derived status ("live" / "cooldown" / "ignored"
// — the engine's own terms) with the timestamp the non-live state lifts. Active is
// false when no bounty is running (the ladder is per-bounty, so there's nothing to
// show or edit then).
type PlayerSanction struct {
	Active        bool
	BountyID      int64
	BountyLabel   string
	ResolveAt     time.Time // bounty win_time — the "ignored" countdown target
	Checks        int
	CooldownUntil *time.Time
	Ignored       bool
	Status        string
}

// sanctionStatus derives the engine's rung name from the persisted columns.
func sanctionStatus(ignored bool, cooldownUntil *time.Time) string {
	if ignored {
		return "ignored"
	}
	if cooldownUntil != nil && time.Now().Before(*cooldownUntil) {
		return "cooldown"
	}
	return "live"
}

// PlayerSanction loads the player's ladder state for the active bounty. With no
// active bounty it returns {Active:false}; with no sanction row (never flagged this
// bounty) it returns a zeroed, "live" state.
func (s *Store) PlayerSanction(ctx context.Context, steamID string) (PlayerSanction, error) {
	b, ok, err := s.ActiveBounty(ctx)
	if err != nil {
		return PlayerSanction{}, err
	}
	if !ok {
		return PlayerSanction{Active: false}, nil
	}
	ps := PlayerSanction{Active: true, BountyID: b.ID, BountyLabel: b.Label, ResolveAt: b.WinTime}
	err = s.pool.QueryRow(ctx, `
		SELECT checks, cooldown_until, ignored
		FROM anticheat_sanctions
		WHERE bounty_id = $1 AND steam_id = $2
	`, b.ID, steamID).Scan(&ps.Checks, &ps.CooldownUntil, &ps.Ignored)
	if errors.Is(err, pgx.ErrNoRows) {
		ps.Status = "live"
		return ps, nil
	}
	if err != nil {
		return PlayerSanction{}, err
	}
	ps.Status = sanctionStatus(ps.Ignored, ps.CooldownUntil)
	return ps, nil
}

// The admin "set flag count" path writes the full ladder state (count + derived
// cooldown/ignored) via SaveSanction — see Engine.SanctionForChecks and the
// adminPlayerChecks handler — so there's no count-only setter here.

// FastestClicker is one row of the fastest-clickers board: a player's mean
// per-round click delta in ms (gap from their previous click that arm; their
// first click of a round measured from the arm) and how many clicks qualified.
type FastestClicker struct {
	SteamID    string
	Name       string
	Clicks     int
	AvgDeltaMs float64
}

// FastestClickers returns one page of the fastest-clickers board, lowest average
// delta (fastest) first, with the total row count for pagination. For the
// all-history view it reads the precomputed fastest_clickers matview (cheap,
// refreshed on a timer). For a bounty window it computes the same metric live,
// restricted to clicks from games that ended in the window (the matview has no
// time dimension, so it can't be filtered).
func (s *Store) FastestClickers(ctx context.Context, w Window, limit, offset int) ([]FastestClicker, int, error) {
	var rows pgx.Rows
	var err error
	if w.All {
		rows, err = s.pool.Query(ctx, `
			SELECT fc.steam_id, p.username, p.display_name, fc.clicks, fc.avg_delta_ms,
			       COUNT(*) OVER()
			FROM fastest_clickers fc
			LEFT JOIN players p ON p.steam_id = fc.steam_id
			ORDER BY fc.avg_delta_ms ASC, fc.steam_id ASC
			LIMIT $1 OFFSET $2
		`, limit, offset)
	} else {
		start, end := w.bounds()
		rows, err = s.pool.Query(ctx, `
			WITH d AS (
				SELECT rs.steam_id,
				       rs.offset_ms - COALESCE(
				           LAG(rs.offset_ms) OVER (
				               PARTITION BY rs.round_id, rs.steam_id ORDER BY rs.slot_no
				           ), 0
				       ) AS delta_ms
				FROM round_scores rs
				JOIN game_rounds r ON r.id = rs.round_id
				JOIN games g ON g.id = r.game_id
				WHERE g.ended_at > $1 AND g.ended_at <= $2
			),
			agg AS (
				SELECT steam_id, COUNT(*) AS clicks, AVG(delta_ms)::DOUBLE PRECISION AS avg_delta_ms
				FROM d GROUP BY steam_id HAVING COUNT(*) >= 10
			)
			SELECT a.steam_id, p.username, p.display_name, a.clicks, a.avg_delta_ms,
			       COUNT(*) OVER()
			FROM agg a LEFT JOIN players p ON p.steam_id = a.steam_id
			ORDER BY a.avg_delta_ms ASC, a.steam_id ASC
			LIMIT $3 OFFSET $4
		`, start, end, limit, offset)
	}
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []FastestClicker{}
	total := 0
	for rows.Next() {
		var f FastestClicker
		var name, disp *string
		if err := rows.Scan(&f.SteamID, &name, &disp, &f.Clicks, &f.AvgDeltaMs, &total); err != nil {
			return nil, 0, err
		}
		f.Name = pickName(name, disp)
		out = append(out, f)
	}
	return out, total, rows.Err()
}

// RefreshFastestClickers recomputes the fastest_clickers materialized view. Run
// on a timer (~10 min). CONCURRENTLY (enabled by the view's unique index) so it
// never blocks concurrent admin reads; it cannot run inside a transaction.
func (s *Store) RefreshFastestClickers(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `REFRESH MATERIALIZED VIEW CONCURRENTLY fastest_clickers`)
	return err
}

// --- Paginated, window-scoped leaderboards for the admin page. These query
// Postgres directly (the in-memory LeaderboardCache only holds the top handful
// of the active window and serves the public API); the admin page needs deep
// pagination and arbitrary (previous) bounty windows.

// PointsBoard returns one page of players by points scored within w (summed
// across hour buckets), highest first, with the total player count.
func (s *Store) PointsBoard(ctx context.Context, w Window, limit, offset int) ([]LeaderboardEntry, int, error) {
	start, end := w.bounds()
	return s.board(ctx, `
		SELECT hs.steam_id, p.username, p.display_name, SUM(hs.points)::INT, COUNT(*) OVER()
		FROM hourly_scores hs
		LEFT JOIN players p ON p.steam_id = hs.steam_id
		WHERE hs.hour_bucket >= $1 AND hs.hour_bucket <= $2
		GROUP BY hs.steam_id, p.username, p.display_name
		ORDER BY SUM(hs.points) DESC, hs.steam_id ASC
		LIMIT $3 OFFSET $4
	`, start.Truncate(time.Hour), end, limit, offset)
}

// HoursWonBoard returns one page of players by UTC clock-hours won within w
// (each completed hour credited to its top scorer), highest first. The
// in-progress hour is excluded.
func (s *Store) HoursWonBoard(ctx context.Context, w Window, limit, offset int) ([]LeaderboardEntry, int, error) {
	start, end := w.bounds()
	currentHour := time.Now().UTC().Truncate(time.Hour)
	if end.Before(currentHour) {
		currentHour = end
	}
	return s.board(ctx, `
		SELECT w.steam_id, p.username, p.display_name, COUNT(*)::INT, COUNT(*) OVER()
		FROM (
			SELECT DISTINCT ON (hour_bucket) hour_bucket, steam_id
			FROM hourly_scores
			WHERE hour_bucket >= $1 AND hour_bucket < $2
			ORDER BY hour_bucket, points DESC, steam_id ASC
		) w
		LEFT JOIN players p ON p.steam_id = w.steam_id
		GROUP BY w.steam_id, p.username, p.display_name
		ORDER BY COUNT(*) DESC, w.steam_id ASC
		LIMIT $3 OFFSET $4
	`, start.Truncate(time.Hour), currentHour, limit, offset)
}

// SessionsWonBoard returns one page of players by games won (placement-1
// finishes in games ended within w), highest first.
func (s *Store) SessionsWonBoard(ctx context.Context, w Window, limit, offset int) ([]LeaderboardEntry, int, error) {
	start, end := w.bounds()
	return s.board(ctx, `
		SELECT gs.steam_id, p.username, p.display_name, COUNT(*)::INT, COUNT(*) OVER()
		FROM game_standings gs
		JOIN games g ON g.id = gs.game_id
		LEFT JOIN players p ON p.steam_id = gs.steam_id
		WHERE gs.placement = 1 AND g.ended_at > $1 AND g.ended_at <= $2
		GROUP BY gs.steam_id, p.username, p.display_name
		ORDER BY COUNT(*) DESC, gs.steam_id ASC
		LIMIT $3 OFFSET $4
	`, start, end, limit, offset)
}

// AllTimeClickersBoard returns one page of players by total scoring clicks in
// games ended within w, highest first. For the all-history window this is the
// lifetime top-clickers board.
func (s *Store) AllTimeClickersBoard(ctx context.Context, w Window, limit, offset int) ([]LeaderboardEntry, int, error) {
	start, end := w.bounds()
	return s.board(ctx, `
		SELECT rs.steam_id, p.username, p.display_name, COUNT(*)::INT, COUNT(*) OVER()
		FROM round_scores rs
		JOIN game_rounds r ON r.id = rs.round_id
		JOIN games g ON g.id = r.game_id
		LEFT JOIN players p ON p.steam_id = rs.steam_id
		WHERE g.ended_at > $1 AND g.ended_at <= $2
		GROUP BY rs.steam_id, p.username, p.display_name
		ORDER BY COUNT(*) DESC, rs.steam_id ASC
		LIMIT $3 OFFSET $4
	`, start, end, limit, offset)
}

// board runs a board query whose final selected column is COUNT(*) OVER() (the
// window-total for pagination) and the preceding four are the scanBoard shape.
func (s *Store) board(ctx context.Context, sql string, args ...any) ([]LeaderboardEntry, int, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []LeaderboardEntry{}
	total := 0
	for rows.Next() {
		var steamID string
		var name, disp *string
		var count int
		if err := rows.Scan(&steamID, &name, &disp, &count, &total); err != nil {
			return nil, 0, err
		}
		p := playerOf(steamID, name, disp)
		out = append(out, LeaderboardEntry{Tag: p.Tag, Username: p.Name(), Points: count, SteamID: steamID})
	}
	return out, total, rows.Err()
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
