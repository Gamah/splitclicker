-- +goose Up
-- +goose StatementBegin

-- -----------------------------------------------------------------------
-- Sessions won — the per-game career board. A "session" is one full game
-- (X rounds); when a game ends, the player who topped its final standings
-- "wins the session" and their tally here is incremented. This is the
-- persistent "who wins the most games" competition, distinct from the
-- hours-won (per-UTC-hour) and within-hour clicks boards.
-- -----------------------------------------------------------------------
CREATE TABLE session_wins (
    steam_id TEXT    PRIMARY KEY,
    wins     INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_session_wins_board ON session_wins (wins DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS session_wins;
-- +goose StatementEnd
