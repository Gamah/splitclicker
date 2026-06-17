-- +goose Up
-- +goose StatementBegin

-- -----------------------------------------------------------------------
-- Fastest clickers — a player's mean "click delta" (arm-to-arrival latency,
-- round_scores.offset_ms) across all their scoring clicks. Materialized (not a
-- live view) and refreshed on a timer (~every 10 min, see main.go) so the
-- admin page stays cheap. A 10-click floor keeps a single lucky click off the
-- board. Lower avg = faster.
-- NOTE: the metric is redefined to a per-round inter-click gap in 00007.
-- -----------------------------------------------------------------------
CREATE MATERIALIZED VIEW fastest_clickers AS
SELECT rs.steam_id,
       COUNT(*)                          AS clicks,
       AVG(rs.offset_ms)::DOUBLE PRECISION AS avg_delta_ms
FROM round_scores rs
GROUP BY rs.steam_id
HAVING COUNT(*) >= 10
WITH DATA;

-- Unique index is required for REFRESH MATERIALIZED VIEW CONCURRENTLY (so the
-- timed refresh never blocks admin reads).
CREATE UNIQUE INDEX idx_fastest_clickers_steam ON fastest_clickers (steam_id);
CREATE INDEX idx_fastest_clickers_avg ON fastest_clickers (avg_delta_ms);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP MATERIALIZED VIEW IF EXISTS fastest_clickers;
-- +goose StatementEnd
