-- +goose Up
-- +goose StatementBegin

-- -----------------------------------------------------------------------
-- Recreate game_standings with a "who got there first" tiebreak.
--
-- The original placement (00005) broke score ties by steam_id ASC — an
-- arbitrary lexicographic order. That view decides placement=1, which is the
-- per-game winner read by the games-won board (SessionsWonLeaderboard) AND by
-- the bounty winner (windowWinner) — i.e. who wins the skin. So a tied game
-- must resolve the same way the live engine does (game/standingsOf): the player
-- who reached the tied total FIRST ranks higher.
--
-- A player's "reached" time is the wire-arrival instant of their LAST scoring
-- click in the game (armed_at + offset_ms), matching the engine's reached[sid]
-- (overwritten on each scoring click, so it ends up as the last). Earlier wins.
-- steam_id stays as a final, stable fallback for the (vanishingly rare) exact
-- timestamp tie.
-- -----------------------------------------------------------------------
DROP VIEW IF EXISTS game_standings;
CREATE VIEW game_standings AS
SELECT r.game_id,
       rs.steam_id,
       COUNT(*)::INT AS points,
       RANK() OVER (PARTITION BY r.game_id
                    ORDER BY COUNT(*) DESC,
                             MAX(r.armed_at + rs.offset_ms * INTERVAL '1 millisecond') ASC,
                             rs.steam_id ASC)::INT AS placement
FROM round_scores rs
JOIN game_rounds r ON r.id = rs.round_id
GROUP BY r.game_id, rs.steam_id;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP VIEW IF EXISTS game_standings;
CREATE VIEW game_standings AS
SELECT r.game_id,
       rs.steam_id,
       COUNT(*)::INT AS points,
       RANK() OVER (PARTITION BY r.game_id
                    ORDER BY COUNT(*) DESC, rs.steam_id ASC)::INT AS placement
FROM round_scores rs
JOIN game_rounds r ON r.id = rs.round_id
GROUP BY r.game_id, rs.steam_id;
-- +goose StatementEnd
