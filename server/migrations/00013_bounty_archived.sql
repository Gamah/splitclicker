-- +goose Up
-- +goose StatementBegin

-- -----------------------------------------------------------------------
-- Archive flag on bounties. An archived bounty is hidden from every
-- client-facing read — it can't become the "active" skin-to-win (so /config
-- reports no active bounty and the HUD shows the "no active bounty" state) and
-- it never appears in the "previous winner" history. It stays fully visible in
-- the admin queue so it can be un-archived. This is purely a visibility flag,
-- orthogonal to the pending → active → won lifecycle: the finalizer still
-- advances an archived bounty's status normally; only client reads filter it
-- out. Lets the host hide live/old bounties to test the empty state cleanly.
-- -----------------------------------------------------------------------
ALTER TABLE bounties ADD COLUMN archived BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE bounties DROP COLUMN archived;
-- +goose StatementEnd
