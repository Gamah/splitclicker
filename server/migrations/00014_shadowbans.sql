-- +goose Up
-- +goose StatementBegin

-- -----------------------------------------------------------------------
-- Shadowbans — the silent ban list.
--
-- A shadowbanned account still connects and still sees the world arm (it
-- receives the `hello` and `armed` frames), but every other frame is withheld
-- and all of its inbound messages are dropped, so it can click forever and
-- never score, win, or appear on a board — with no error that would reveal the
-- ban. Enforcement is at the WS connect path (a per-connect primary-key lookup),
-- so a ban takes effect on the player's NEXT connect; the admin "drop" control
-- forces that reconnect.
--
-- No FK to players: an account can be banned before it has ever played, and the
-- list is a moderation control, not a scoring relation.
-- -----------------------------------------------------------------------

CREATE TABLE shadowbans (
    steam_id   TEXT        PRIMARY KEY,
    reason     TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS shadowbans;
-- +goose StatementEnd
