-- +goose Up
-- +goose StatementBegin

-- -----------------------------------------------------------------------
-- display_name — the player's Steam display name, reported by the client at
-- auth time. Unlike username (a claimed, validated, UNIQUE handle) this is
-- whatever Steam says: not unique, not validated, purely cosmetic. It lets the
-- board show real Steam names instead of the opaque hex tag when a player
-- hasn't claimed a username.
-- -----------------------------------------------------------------------
ALTER TABLE players ADD COLUMN display_name TEXT;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE players DROP COLUMN display_name;
-- +goose StatementEnd
