-- +goose Up
-- +goose StatementBegin

-- -----------------------------------------------------------------------
-- Game replays — the admin replay viewer's per-game visualization data.
--
-- One row per recorded game: a gzipped JSON blob (game.GameReplay) holding,
-- for every round, the live buttons, the claims, and each player's cursor path
-- — all keyed to milliseconds from the round's arm so the viewer plays them on
-- one timeline. Written in the SAME transaction as the game history (games /
-- game_rounds / round_scores) by store.RecordGame, so a replay that fails to
-- write rolls the whole game back: a game on the leaderboards always has its
-- replay, and vice versa.
--
-- The blob is small (single-digit KB gzipped even for a full crowd) and only
-- ever read back whole, so it lives inline rather than as a per-sample table.
-- ON DELETE CASCADE so pruning a game drops its replay with it.
-- -----------------------------------------------------------------------

CREATE TABLE game_replays (
    game_id UUID  PRIMARY KEY REFERENCES games(id) ON DELETE CASCADE,
    data    BYTEA NOT NULL
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS game_replays;
-- +goose StatementEnd
