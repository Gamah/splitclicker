-- +goose Up
-- +goose StatementBegin

-- -----------------------------------------------------------------------
-- Inspect links on bounties. A bounty's skin can now be specified by a CS2
-- inspect link instead of (or alongside) an uploaded image. The link self-
-- encodes the item (defindex/paintindex/paintseed/float as of Valve's March
-- 2026 change), so the client decodes it locally and renders the live float,
-- paint seed, name and a wear bar, resolving the weapon image from a community
-- dataset. The uploaded skin_image remains the fallback shown when the client
-- can't fetch/decode the link, so it is no longer strictly required.
-- -----------------------------------------------------------------------
ALTER TABLE bounties ADD COLUMN inspect_link TEXT NOT NULL DEFAULT '';

-- skin_image was NOT NULL with no default; a link-only bounty stores '' there.
-- The column is already nullable-free, so '' is the link-only sentinel.

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE bounties DROP COLUMN inspect_link;
-- +goose StatementEnd
