# data/ — host-editable runtime config

The skin and timestamp here are read off disk **on every request**, so editing
them applies live — no rebuild, no restart. In the Docker stack this whole
directory is bind-mounted into the container at `/data` (see
`docker/docker-compose.yml`).

## Edit config.json

1. Copy the example once: `cp config.json.example config.json`
2. Edit `config.json`:
   ```json
   {
     "skin_image": "skin2win.png",
     "winner_lock_time": "2026-06-16T07:00:00Z",
     "dev_note": "",

     "live_version": 4,

     "arm_min_sec": 2,
     "arm_max_sec": 6,
     "clicks_per_player": 15,
     "min_clicks": 50,
     "rounds_per_game": 5,
     "race_max_ms": 5000,
     "result_display_ms": 4000,
     "intermission_ms": 5000,
     "board_size": 20,
     "tick_hz": 20,
     "tick_sample_k": 8,
     "fast_click_ms": 130,
     "max_click_factor": 2.5,
     "solo_lead_margin": 15,
     "dominant_runner_up_min": 5,
     "check_cooldown_threshold": 20,
     "check_cooldown_mins": 60,
     "check_ignore_after": 2
   }
   ```

**Apply live (re-read per request):**
- `skin_image` — a filename inside `media/` (served at `GET /api/v1/skin`).
- `winner_lock_time` — RFC3339; the client counts down to it. Empty/omit hides
  the countdown. Once it passes, the HUD prompts for a new skin.
- `dev_note` — a broadcast message shown orange on every client (under the
  throttle line). Re-read once at the start of each game, so a change takes
  effect on the next game; empty/omit clears it (no restart needed).
- `live_version` — the current "live" client API version (integer). Clients on a
  lower version get the troll leaderboards + an "out of date" note; live-or-newer
  are respected. Bump it to disable an old build (e.g. set `4` once v4 is rolled
  out), or leave it below a new build's version to test that build alongside the
  live one. Omit ⇒ default `4`.

**Game tunables (read at startup → `docker compose restart app` to apply):**
- `arm_min_sec` / `arm_max_sec` — random arming-delay window.
- `clicks_per_player` / `min_clicks` — N = clicks_per_player × players, floored
  at min_clicks.
- `rounds_per_game`, `race_max_ms`, `result_display_ms`, `intermission_ms`,
  `board_size`.
- `tick_hz` — live-window broadcast cadence: while the button is armed the server
  pushes the clicks-remaining count + a sampled batch of opponent pips this many
  times a second (default 20; `0` disables the tick entirely). `tick_sample_k` —
  max opponent pips sampled per tick (default 8); the remaining count is always
  exact, only the positioned-pip sample is capped, which is what keeps tick egress
  linear in player count.
- `penalty_base_ms` / `penalty_step_ms` — idle-click arm-delay escalation.
- `fast_click_ms` — anticheat: two consecutive scoring clicks closer than this
  (default 130) flag the player. `max_click_factor` — anticheat: more than
  `max_click_factor ×` the round's fair share (N ÷ the players who actually scored
  this round) of the scoring clicks flags them (default 2.5; fractional allowed, the
  limit floors to a whole click count); needs ≥2 scorers, so a round only one player
  clicked is never flagged.
- `solo_lead_margin` — anticheat: solo_round only flags a lone leader once their
  games-won lead over second place is at least this (default 15).
- `dominant_runner_up_min` — anticheat: dominant_winner only fires when the
  runner-up scored at least this many clicks, so out-clicking an idle player is
  never flagged (default 5).
- `check_cooldown_threshold` / `check_cooldown_mins` / `check_ignore_after` — the
  per-bounty sanction ladder: this many flags in a bounty (default 20) start a
  cooldown of this many minutes (default 60); this many more flags after that
  (default 2) sideline the player until the bounty resolves. Counts reset each
  bounty.

Any omitted field falls back to its env var (e.g. `SKIN_IMAGE`, `ARM_MIN_SEC`),
then the built-in default. `config.json` is gitignored (the live, host-owned
file); `config.json.example` is the committed template.

## Change the skin image bytes

Drop a new file into `media/` and point `skin_image` at it (or overwrite the
existing file). Served immediately.
