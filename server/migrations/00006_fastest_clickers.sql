-- +goose Up
-- +goose StatementBegin

-- -----------------------------------------------------------------------
-- Fastest clickers — a player's mean "click delta" across all their scoring
-- clicks. The delta is per-round (reset each arm), measured from their previous
-- click in that round; their FIRST click in a round is measured from the arm
-- (offset_ms). Equivalently: treat the arm as a virtual click at offset 0 per
-- player per round, so every delta is offset_ms - LAG(offset_ms) partitioned by
-- (round_id, steam_id). This captures real clicking/mashing speed and ignores
-- the long dead time between rounds. Materialized (not a live view) and
-- refreshed on a timer (~every 10 min, see main.go) so the admin page stays
-- cheap. A 10-click floor keeps a single lucky click off the board. Lower = faster.
-- -----------------------------------------------------------------------
CREATE MATERIALIZED VIEW fastest_clickers AS
SELECT steam_id,
       COUNT(*)                   AS clicks,
       AVG(delta_ms)::DOUBLE PRECISION AS avg_delta_ms
FROM (
    SELECT rs.steam_id,
           rs.offset_ms - COALESCE(
               LAG(rs.offset_ms) OVER (
                   PARTITION BY rs.round_id, rs.steam_id ORDER BY rs.slot_no
               ), 0
           ) AS delta_ms
    FROM round_scores rs
) d
GROUP BY steam_id
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
