-- +goose Up
-- +goose StatementBegin

-- -----------------------------------------------------------------------
-- Redefine fastest_clickers: from a player's mean arm-to-arrival latency
-- (00006) to their mean per-round INTER-CLICK gap. The delta resets each arm
-- and is measured from their previous click in that round; their FIRST click of
-- a round is measured from the arm (offset_ms). Equivalently: treat the arm as a
-- virtual click at offset 0 per player per round, so every delta is
-- offset_ms - LAG(offset_ms) partitioned by (round_id, steam_id). This captures
-- real clicking/mashing speed and ignores the long dead time between rounds.
--
-- A materialized view's columns can't be altered in place, so drop + recreate
-- (the unique index it needs for REFRESH ... CONCURRENTLY goes with it). Output
-- columns are unchanged, so the Go read path and refresh loop are untouched.
-- -----------------------------------------------------------------------
DROP MATERIALIZED VIEW IF EXISTS fastest_clickers;

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

CREATE UNIQUE INDEX idx_fastest_clickers_steam ON fastest_clickers (steam_id);
CREATE INDEX idx_fastest_clickers_avg ON fastest_clickers (avg_delta_ms);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Revert to the 00006 definition (mean arm-to-arrival latency).
DROP MATERIALIZED VIEW IF EXISTS fastest_clickers;

CREATE MATERIALIZED VIEW fastest_clickers AS
SELECT rs.steam_id,
       COUNT(*)                          AS clicks,
       AVG(rs.offset_ms)::DOUBLE PRECISION AS avg_delta_ms
FROM round_scores rs
GROUP BY rs.steam_id
HAVING COUNT(*) >= 10
WITH DATA;

CREATE UNIQUE INDEX idx_fastest_clickers_steam ON fastest_clickers (steam_id);
CREATE INDEX idx_fastest_clickers_avg ON fastest_clickers (avg_delta_ms);

-- +goose StatementEnd
