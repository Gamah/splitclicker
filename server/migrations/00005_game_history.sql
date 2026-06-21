-- +goose Up
-- +goose StatementBegin

-- -----------------------------------------------------------------------
-- Game history — the durable, queryable record of every completed game,
-- written once at game end (never on the hot path). Aggregates like
-- hourly_scores/session_wins are the live boards; these tables are the
-- after-the-fact log you can query to reconstruct what happened.
-- -----------------------------------------------------------------------

-- One row per completed game (X rounds).
CREATE TABLE games (
    id         UUID        PRIMARY KEY,   -- engine gameID
    started_at TIMESTAMPTZ NOT NULL,
    ended_at   TIMESTAMPTZ NOT NULL,
    rounds     INTEGER     NOT NULL       -- X (rounds in the game)
);

-- One row per round of a game.
CREATE TABLE game_rounds (
    id        UUID        PRIMARY KEY,    -- engine roundID
    game_id   UUID        NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    round_no  INTEGER     NOT NULL,
    n         INTEGER     NOT NULL,       -- scoring slots this round
    players   INTEGER     NOT NULL,       -- connected at arm
    armed_at  TIMESTAMPTZ NOT NULL,
    UNIQUE (game_id, round_no)
);
CREATE INDEX idx_game_rounds_game ON game_rounds (game_id);

-- One row per SCORING CLICK: slot_no is "click N" (0..n-1, arrival order),
-- offset_ms is wire-arrival latency from the arm (click At minus armed_at;
-- so the absolute arrival is armed_at + offset_ms).
CREATE TABLE round_scores (
    round_id  UUID    NOT NULL REFERENCES game_rounds(id) ON DELETE CASCADE,
    slot_no   INTEGER NOT NULL,           -- the "click N" within this round
    steam_id  TEXT    NOT NULL REFERENCES players(steam_id),
    offset_ms INTEGER NOT NULL,
    PRIMARY KEY (round_id, slot_no)
);
CREATE INDEX idx_round_scores_player ON round_scores (steam_id);
CREATE INDEX idx_round_scores_round  ON round_scores (round_id);

-- Final per-game standings + placement, DERIVED (not stored — keeps it 3NF).
-- A player's points = the number of slots they took across the game; placement
-- is points desc, steam_id asc tiebreak. (00012 later recreates this view to
-- break ties by who reached the total first, matching the engine's standingsOf.)
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

-- +goose Down
-- +goose StatementBegin
DROP VIEW IF EXISTS game_standings;
DROP TABLE IF EXISTS round_scores;
DROP TABLE IF EXISTS game_rounds;
DROP TABLE IF EXISTS games;
-- +goose StatementEnd
