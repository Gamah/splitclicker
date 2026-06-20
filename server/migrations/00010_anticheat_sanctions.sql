-- +goose Up
-- +goose StatementBegin

-- -----------------------------------------------------------------------
-- Anticheat v4 — per-check client message + the per-bounty sanction ladder.
--
-- Two additions:
--   1. anticheat_checks.message — the human-readable line shown to the player
--      when a check benches them ("Clicking faster than humanly possible."),
--      so the client can tell them WHICH rule fired, not just the audit detail.
--   2. anticheat_sanctions — the escalation state scoped to (bounty, player).
--      Every failed check increments `checks`. Crossing CheckCooldownThreshold
--      starts a timed cooldown (`cooldown_until`); CheckIgnoreAfter more checks
--      past that flips `ignored`, which sidelines the player until the bounty
--      resolves. The row is keyed per bounty so the ladder resets each bounty.
-- -----------------------------------------------------------------------

ALTER TABLE anticheat_checks
    ADD COLUMN message TEXT NOT NULL DEFAULT '';

-- One row per (bounty, player) holding their escalation state for that bounty.
-- cooldown_until is NULL until they cross the cooldown threshold; ignored stays
-- false until they earn enough checks after the cooldown. The engine loads the
-- active bounty's rows on startup / bounty change and upserts as checks accrue.
CREATE TABLE anticheat_sanctions (
    bounty_id      BIGINT      NOT NULL REFERENCES bounties(id) ON DELETE CASCADE,
    steam_id       TEXT        NOT NULL REFERENCES players(steam_id),
    checks         INTEGER     NOT NULL DEFAULT 0,
    cooldown_until TIMESTAMPTZ,                          -- NULL until the cooldown threshold is crossed
    ignored        BOOLEAN     NOT NULL DEFAULT false,   -- sidelined until the bounty resolves
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (bounty_id, steam_id)
);

CREATE INDEX idx_anticheat_sanctions_bounty ON anticheat_sanctions (bounty_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS anticheat_sanctions;
ALTER TABLE anticheat_checks DROP COLUMN IF EXISTS message;
-- +goose StatementEnd
