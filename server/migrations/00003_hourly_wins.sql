-- +goose Up
-- +goose StatementBegin

-- -----------------------------------------------------------------------
-- Hours won — the career board. When a UTC clock-hour completes, the player
-- who topped that hour's hourly_scores "wins" the hour; their tally here is
-- incremented. This is the persistent "who wins the most hours" competition,
-- distinct from the within-hour clicks board.
-- -----------------------------------------------------------------------
CREATE TABLE hourly_wins (
    steam_id TEXT    PRIMARY KEY,
    hours    INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_hourly_wins_board ON hourly_wins (hours DESC);

-- -----------------------------------------------------------------------
-- Finalized hours — idempotency ledger for the hour finalizer. An hour is
-- recorded here once its winner has been credited, so a restart (or a second
-- finalizer pass) never double-counts a win.
-- -----------------------------------------------------------------------
CREATE TABLE finalized_hours (
    hour_bucket TIMESTAMPTZ PRIMARY KEY
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS finalized_hours;
DROP TABLE IF EXISTS hourly_wins;
-- +goose StatementEnd
