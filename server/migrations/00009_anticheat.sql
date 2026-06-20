-- +goose Up
-- +goose StatementBegin

-- -----------------------------------------------------------------------
-- Anticheat — per-round "checks" and the "tests" they trigger.
--
-- At the end of every round the engine runs checks against each player's
-- scoring clicks (suspiciously fast inter-click deltas; an implausible click
-- count). Every flagged check is recorded here keyed to the round, so the
-- admin surface can see which rounds produced checks and against whom.
--
-- A failed check on a test-capable client benches that player until they pass
-- a "test" (currently the sum of two 2-digit numbers). Every test sent and the
-- answer received is recorded for audit.
-- -----------------------------------------------------------------------

-- One row per check a round flagged against a player. round_id references the
-- durable game_rounds row written in the same game-history transaction, so the
-- presence of rows for a round is exactly "this round added checks".
CREATE TABLE anticheat_checks (
    id         BIGSERIAL   PRIMARY KEY,
    round_id   TEXT        NOT NULL REFERENCES game_rounds(id) ON DELETE CASCADE,
    steam_id   TEXT        NOT NULL REFERENCES players(steam_id),
    check_type TEXT        NOT NULL,                 -- 'fast_clicks' | 'too_many_clicks'
    detail     TEXT        NOT NULL DEFAULT '',      -- e.g. 'delta=84ms' / 'clicks=37'
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_anticheat_checks_round  ON anticheat_checks (round_id);
CREATE INDEX idx_anticheat_checks_player ON anticheat_checks (steam_id);

-- One row per test sent to a flagged player. id is the engine-generated token a
-- correct answer must echo; answer/correct/answered_at stay NULL until the
-- player responds. A wrong answer settles the row (correct=false) and a fresh
-- test row is issued, so the table is the full audit trail of every attempt.
CREATE TABLE anticheat_tests (
    id          TEXT        PRIMARY KEY,             -- engine token (newID)
    steam_id    TEXT        NOT NULL REFERENCES players(steam_id),
    test_kind   TEXT        NOT NULL,                -- 'sum2'
    prompt      TEXT        NOT NULL,                -- '47 + 38'
    expected    TEXT        NOT NULL,                -- '85'
    answer      TEXT,                                 -- received answer (NULL until answered)
    correct     BOOLEAN,                              -- NULL until answered
    sent_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    answered_at TIMESTAMPTZ
);

CREATE INDEX idx_anticheat_tests_player ON anticheat_tests (steam_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS anticheat_tests;
DROP TABLE IF EXISTS anticheat_checks;
-- +goose StatementEnd
