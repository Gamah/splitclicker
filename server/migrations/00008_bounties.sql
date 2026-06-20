-- +goose Up
-- +goose StatementBegin

-- -----------------------------------------------------------------------
-- Bounties — the managed sequence of skins-to-win. A "bounty" is one skin
-- offered for a timeframe; when its win_time passes, the player who won the
-- most GAMES during that window (its activation → win_time) is recorded as
-- the bounty's winner and the next queued bounty activates automatically, so
-- the game keeps ticking forward. This is the DB-backed replacement for the
-- single config.json skin_image + winner_lock_time pair.
--
-- Exactly one bounty is 'active' at a time (the partial unique index below);
-- 'pending' bounties form the queue (promoted in win_time order); 'won'
-- bounties are the settled history with their winner.
-- -----------------------------------------------------------------------
CREATE TABLE bounties (
    id           BIGSERIAL   PRIMARY KEY,
    skin_image   TEXT        NOT NULL,                 -- base filename in the media dir
    label        TEXT        NOT NULL DEFAULT '',      -- admin's name for the skin
    win_time     TIMESTAMPTZ NOT NULL,                 -- deadline; winner is locked when it passes
    status       TEXT        NOT NULL DEFAULT 'pending' -- pending | active | won
                 CHECK (status IN ('pending', 'active', 'won')),
    activated_at TIMESTAMPTZ,                           -- when it became active = window start
    winner_id    TEXT        REFERENCES players(steam_id),
    winner_name  TEXT        NOT NULL DEFAULT '',       -- display-name snapshot at finalize
    winner_wins  INTEGER     NOT NULL DEFAULT 0,        -- games the winner won during the window
    won_at       TIMESTAMPTZ,                           -- when it was finalized
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- At most one active bounty at any time.
CREATE UNIQUE INDEX idx_bounties_one_active ON bounties (status) WHERE status = 'active';

-- Queue order for promotion (earliest deadline first) and the history view.
CREATE INDEX idx_bounties_pending ON bounties (win_time, id) WHERE status = 'pending';
CREATE INDEX idx_bounties_won     ON bounties (won_at DESC) WHERE status = 'won';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS bounties;
-- +goose StatementEnd
