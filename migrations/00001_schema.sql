-- +goose Up
-- +goose StatementBegin

-- -----------------------------------------------------------------------
-- Players — one row per Steam account we've seen. steam_id is the
-- authoritative identity (validated via Facepunch); username is optional.
-- -----------------------------------------------------------------------
CREATE TABLE players (
    steam_id   TEXT        PRIMARY KEY,
    username   TEXT        UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- -----------------------------------------------------------------------
-- Hourly leaderboard — the persistent "most clicks" board. One row per
-- (player, clock-hour); points accumulate within the UTC hour. The board is
-- read for the current hour_bucket, so it "resets" simply by the bucket
-- advancing on the clock hour.
-- -----------------------------------------------------------------------
CREATE TABLE hourly_scores (
    steam_id    TEXT        NOT NULL,
    hour_bucket TIMESTAMPTZ NOT NULL,
    points      INTEGER     NOT NULL DEFAULT 0,
    PRIMARY KEY (steam_id, hour_bucket)
);

-- Read path: WHERE hour_bucket = $now ORDER BY points DESC LIMIT k.
CREATE INDEX idx_hourly_scores_board ON hourly_scores (hour_bucket, points DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS hourly_scores;
DROP TABLE IF EXISTS players;
-- +goose StatementEnd
