# CLAUDE.md — Splitclicker Project Context

This file is the authoritative context document for Claude sessions working on this repo.
Read it fully before making any changes. The detailed design lives in **[PLAN.md](PLAN.md)** —
this file is the quick orientation; PLAN.md is the source of truth for architecture decisions.

**Status:** both halves are built out. `server/` has the engine state machine, WS hub,
Facepunch auth, the bounty + leaderboard boards, the anticheat checks + per-bounty
sanction ladder, Docker/Caddy, and unit tests. `client/` is a playable s&box game:
`ClickController` (WS lifecycle/phase), the single-root `Hud.razor` (boards, the
multi-button board + opponent cursors, GAME INFO popup, anticheat overlays), the
Skafinity music library, and s&box-Services achievements. **The live API version is
`v5`** (the multi-button board + opponent cursors, on top of the live-window tick);
the config-driven `live_version` floor is now `v5`, with `v4` (single persistent
button, no tick) as the supported N-1.

---

## What is Splitclicker?

A competitive, global, real-time clicker game built as an **s&box** game with a **Go**
backend. There is a single global button that is *only clickable* for a brief burst after a
random "arming" delay. When the button arms, the first **N** clicks worldwide each score a
point; then it hard-disables until the next arm. The name: the clicks are "split" across a
global field of players racing the same button.

### Core rules
- A **game** = **X rounds**. Each round: random arm delay (default 10–120s) → button arms →
  the first **N** clicks worldwide each score +1 → button disables → leaderboard shown → next
  round. The window stays open until all **N** clicks are consumed (or RaceMax fires).
- After X rounds → `game_over` (final standings), brief intermission, fresh game.
- Points accumulate into an **hourly leaderboard** (UTC) that resets on the clock hour — the
  persistent "most clicks" board.
- **No per-player dedupe within an arm:** a fast clicker can take several of the N slots in one
  arm (mashing inside the live window is rewarded; the per-connection rate limiter bounds it).
- `N=1` = pure first-click-wins; `N>1` = multiple scoring clicks per arm. All numbers tunable.

### The hard constraint: latency fairness
Rounds are decided by sub-frame click timing, so the click path must **not** go through the
s&box engine tick. Clicks go straight to the Go backend over WebSocket; **server wire-arrival
order is truth** (nanosecond stamps, no tick quantization).

---

## Architecture (one process, one button)

```
s&box client ──WS /ws?ticket=──► Go backend (sole authority)
   (thin UI)  ◄──broadcasts────  ├─ WS hub (single precomputed broadcast)
              ──HTTP /api/v1/──►  ├─ game state machine (arm RNG + race + score)
                                  ├─ Postgres (players, hourly_scores)
                                  └─ Facepunch token validation → public.facepunch.com
```

- **One authoritative Go process, one global button, one WS server.** No s&box dedicated
  server, no Go sharding — a single box holds thousands of cheap idle WS connections.
  Horizontal fan-out is a *documented escape hatch only* (keep the broadcast behind an
  interface); do not build it now.
- **Identity = Steam.** Reuse rotaliate's Facepunch-token validation (`internal/steam`):
  validate `{steamid, token}` against `public.facepunch.com/sbox/auth/token` (fail-closed),
  then trust the reported SteamID. Threat model is narrow: the only realistic abuse is
  clicking on someone else's behalf, which is acceptable.
  - **No client-supplied username (decided #28).** There is no claimable handle and no
    claim UI; the rotaliate-inherited `username` machinery was vestigial dead weight and the
    root cause of the 422 reconnect loop (#25). `POST /auth` ignores any inbound `username`
    (so a stuck pre-fix client self-heals — there's no username path left to 422 on),
    `session.ValidateUsername` + the reserved/profanity lists are gone, and the board name is
    always the sanitized Steam **display name**. The `players.username` column is kept
    nullable-and-ignored (no migration; door left open for a real claim flow later); it stays
    NULL, so `session.PlayerTag(steamID, "")` == `sha256(steamID)` and **all existing tags are
    unchanged**. Claimable usernames are *not* a current feature.

---

## Stack (planned — mirrors the rotaliate family)

| Layer | Tech |
|---|---|
| Backend | Go (1.22+) |
| Database | PostgreSQL 16 |
| Real-time | WebSockets (prefer an **epoll-based** lib — `nbio`/`gobwas/ws` — over goroutine-per-conn, for idle-conn scale) |
| Migrations | goose (filesystem path, NOT embed) |
| Client | s&box (C#, Razor UI), thin HTTP/WS front-end |
| Deploy | Docker Compose, Caddy reverse proxy (TLS; **WSS** required) |

---

## Sibling repos (pattern sources — read, don't reinvent)

This project deliberately reuses proven patterns from the rotaliate family. When implementing,
copy the shape from these (paths relative to the workspace root, e.g. `../rotaliate`):

Server (`../rotaliate`):
- `internal/steam/auth.go` — `ValidateToken` (copy ~verbatim — same Facepunch path).
- `internal/ws/hub.go` — WS hub, broadcast, rate-limit timestamps.
- `internal/api/router.go` — Go 1.22 ServeMux, WS-ticket endpoint.
- `internal/solo/` — server-authoritative session + token-bucket move limiter.
- `internal/db/`, `migrations/` — pgx pool + goose-from-filesystem.
- `Makefile`, `docker/` — compose, Caddy, `GIT_HASH` version-stamp conventions.

Client (`../rotaliate-client`):
- `rotaliate/Code/Api/ApiClient.cs` — `Http.RequestAsync`, `Sandbox.Services.Auth.GetToken`,
  headers, WS-ticket mint.
- `rotaliate/Code/Ws/WsClient.cs` — `Sandbox.WebSocket` component wrapper.
- `rotaliate/Code/Game/PlayerData.cs` — `FileSystem.Data` identity persistence.
- `rotaliate/Code/UI/Screens/LeaderboardScreen.razor` — list UI (`@foreach`, `StateHasChanged`).
- `rotaliate/Code/UI/Screens/ModePickerScreen.razor` — clickable button pattern.
- `rotaliate/rotaliate.sbproj` — project metadata fields.

s&box engine docs: `../sbox-docs` (e.g. `docs/services/{auth-tokens,achievements,stats}.md`).

---

## Repo layout

This is a **monorepo**: the Go backend lives under `server/` and the s&box game
under `client/`, with a single root `README.md`. (This supersedes the original
plan of a separate `splitclicker-client` repo — kept as one repo at the user's
direction.)

```
server/                # Go backend (module github.com/gamah/splitclicker)
  cmd/server/          # main entrypoint
  internal/
    steam/             # Facepunch token validation (copied from rotaliate)
    session/           # public player tag (SteamID-derived; no username — see #28)
    game/              # round/game state machine: arm RNG, race, scoring (first N by arrival)
    store/             # Postgres: players, leaderboard boards, bounties, anticheat (pgx)
    ws/                # WS hub: registry, single precomputed broadcast, click ingestion
    api/               # REST: /auth, /leaderboard/*, /config, /admin/*, /health, + /ws upgrade
    db/                # pgx pool + goose migrations (filesystem)
  migrations/          # goose SQL files
  docker/              # Dockerfile + compose (app on 6969; external Caddy fronts it)
client/                # s&box game (splitclicker.sbproj + Code/)
PLAN.md                # full design & architecture (source of truth)
```

Run Go tooling from `server/` (the module root). The s&box project is `client/`.

---

## Key design decisions (see PLAN.md for full rationale)

- **Anti-pre-fire via per-arm nonce.** Each arm carries a random `armNonce` revealed only in
  the `armed` frame; a scoring click must echo it. A client physically cannot form a valid
  click before receiving `armed` — defeats blind flooding *and* is the prerequisite that makes
  the spam-penalty work.
- **Spam deterrent = delayed arm.** Idle clicks (button dormant) are allowed but penalize that
  connection: hold back its next `armed` frame by an escalating delay — the Nth idle click adds
  N×5ms, so the held penalty grows 5,15,30,50,75,105… ms (`step·N(N+1)/2`), reset each round.
  Fixed formula (not env-configurable); mirrored client-side (`ClickController.IdlePenaltyMs`) for
  a live estimate that the authoritative `armed` value overwrites. Mashing becomes self-defeating.
  Idle clicks still rate-limited so this doesn't reintroduce idle traffic.
- **Anticheat checks + sanction ladder** (`game.runChecks` / `game.checkSoloSession` /
  `game.applySanction`; tunables in `data/config.json`). End of every round, the scoring clicks
  are inspected by `runChecks`: **fast_clicks** (sub-human inter-click gap), **too_many_clicks**
  (over `max_click_factor ×` the round's fair share N÷(players who scored this round); needs ≥2
  scorers, so a lone clicker is never flagged), **dominant_winner** (>2× a runner-up who scored
  ≥`dominant_runner_up_min`, so beating an idle player is safe). Two **cursor checks** (v5 only)
  use the per-window cursor activity supplied by `ws/hub.go`'s `Hub.CursorActivity` via
  `Engine.SetCursorActivityFn`, both gated by `afk_cursor_min`>0, both per-player (catch a lone
  automated clicker), and both **skip legacy/disconnected players, never flagging them** (a v4
  client sends no cursors): **afk** (scored while *no* `cursor` frames arrived — window backgrounded;
  the soft, vague flag, message "This is not an AFK game.") and **moveless_score** (a scoring click
  while AFK by the cursor — frames *did* arrive but the pointer's bounding box spanned <
  `afk_cursor_min` normalized units, a frozen cursor claiming buttons that spawn at server-RNG'd
  positions: the deliberate-automation signature, so it's a **distinct check type** flagged separately
  in the audit/sanction trail. Its player message is deliberately vague ("Nice try.") — it must NOT
  reveal that cursor movement is what's measured). **solo_round** is the one
  *session-level* check (`game.checkSoloSession`, evaluated once at game end, NOT per round):
  it flags the bounty leader for padding a runaway lead only when the session was **uncontested**
  — the leader was the *only* player to score in *any* round (a single scoring click from anyone
  else makes it contested and the lead stands) — and the leader's lead **after** winning it
  (start-of-session margin +1, since the sole scorer wins) strictly exceeds `solo_lead_margin`
  (so it first fires at a lead of 5 with the default 4). Each carries a player-facing message.
  Flags escalate
  **per-bounty** (counts reset each bounty, persisted in `anticheat_sanctions`): test (math) →
  cooldown (`check_cooldown_threshold` flags → `check_cooldown_mins`) → ignored
  (`check_ignore_after` more → until the bounty resolves). The server pushes the rung as a `test`
  frame with `state`/`message`/`until_ms`; the client shows the test, then a countdown. The
  bounty snapshot the checks need (id, leader+margin, resolve time) is supplied via
  `Engine.SetBountyInfoFn` from the leaderboard cache. Every leaderboard row + the WS
  standings also carry a `status` field (`live`/`cooldown`/`ignored`) for a coloured dot —
  stamped at serve time from `Engine.SanctionStatuses()` (HTTP boards) / `annotateStatus`
  (WS standings); additive, so older clients ignore it.
- **API versioning.** REST/WS are versioned (`/api/{ver}`, `/ws/{ver}`); the live floor is
  config-driven (`live_version`). Below-live clients get the troll boards + "out of date" note;
  live-or-newer are respected (so a new build is testable before the floor moves up). The sole
  capability gate now is `ws/hub.go`'s `minTickVersion` (v5: tick/roster/click-x-y) — with the
  floor at v4 every non-legacy conn is sanction-capable, so `TestCapable`/`SanctionCapable`
  collapsed to `!Legacy`. **Cleanup rule:** support only N and N-1 — once a new build goes live,
  prune handling two+ versions back (when v6 is live, drop all v4-and-older special-casing,
  collapsing `minTickVersion` the same way). See the note at `api/router.go`'s `liveVersionDefault`.
- **Multi-button live window + opponent cursors (v5).** An armed window shows up to
  `ButtonsOnScreen` (X, default 10) live buttons at once (`game/board.go`). **1 button = 1 point**:
  the first valid click echoing a button's nonce claims it (+1), the button is consumed, and — while
  the round still has budget — a replacement spawns so the board stays refilled to X. The round still
  ends the instant **N** total points are claimed (N is the crowd-scaled budget, X is just visual
  density), so up to X−1 unclaimed buttons can be on screen at the final claim; they're discarded.
  *(Future option, documented not built: shrink the board to `min(X, remaining)` as it drains.)*
  - **Positions are server-authoritative and transmitted, never client-derived.** The initial X ride
    the `armed` frame; each replacement's offset is server-RNG'd (`Engine.randPos`, `crypto`-seeded
    `math/rand`, non-overlapping) and shipped in its tick claim event. There is **no shared seed /
    deterministic generator** — a client cannot pre-compute where buttons appear and pre-aim.
  - **Tick (binary, `TickHz`×/s)** carries `remaining` + **every** board mutation since the last tick
    (`{slot, claimer_tag, t_arm, spawn?{id,nonce,x,y}}` — authoritative/**complete**, never sampled, or
    a client would miss a live button) + a **sample** of opponent cursors (`{tag,x,y}`, capped at
    `TickSampleK`). Mutation bytes are O(scoring clicks), never per-non-scoring-click.
  - The client draws each button at its position, renders a claimed-button pip (sound a fifth up, half
    volume) labelled with the claimer's username (resolved from the `round_pending` roster), and replays
    pips at their true moment via the jitter buffer (tail drains over `round_result`, cleared at the next
    arming stage). The pip position now comes from the **claimed button** (not the click's x/y).
  - **Cursors**: client→server `cursor {x,y}` (~15/s, throttled `cursorMinGap`), **armed-only**, cleared
    at the arming stage (`Hub.Pending`) and on round/game end. New inbound type; no shared WS click
    bucket exists today, but any future one must budget cursors separately (else a moving mouse starves
    clicks).
  - **v4 (below-v5) clients**: the engine also mints a single **persistent legacy nonce** button (scores
    into the same N budget, never consumed/replaced, no board mutation), sent as the lone `armed.nonce`
    so an old client keeps today's single-button play with no new frames. During the v4↔v5 coexistence
    window a v4 player's stationary button is easier than v5's moving board — **benign and short-lived**
    (gone at cutover), not designed around.
  - **`fast_clicks` tension**: sweeping X buttons fast is now intended play; current `FastClickMs`
    default/prod config is kept, flagged here to retune later if it false-positives.
- **Traffic minimization.** Persistent WS, never polling. Idle is silent. Cheap idle conns
  (epoll lib), infrequent/long heartbeats, one precomputed broadcast on arm, race closes the
  instant click N lands (losing clicks read-and-dropped), leaderboard pushed inside
  `round_result` (never a fan-in `GET` stampede), jittered reconnect backoff.
- **Bounty rollover is pushed, not polled.** When `runBountyFinalizer` flips the active bounty
  (the win_time passes → winner recorded → next promoted), it broadcasts a payload-less
  `bounty_update` frame; the client re-fetches `/config` (new skin/countdown) + `/bounties/previous`
  (the just-settled winner) on that push, on every `hello` (so a reconnect across a rollover also
  refreshes), and on load — driven by `ClickController.BountyRefreshSeq`, never a timer. This is
  the cache-invalidation fix for the stale post-rollover HUD. `GET /api/{ver}/bounties/previous`
  returns up to the 5 most-recent won bounties (winner tag/steamid/name/wins + inspect link +
  per-bounty `skin_url`); `GET /api/{ver}/skin/{id}` serves a specific past bounty's image. The
  client shows the latest as a display-only "PREVIOUS WINNER" panel mirroring the skin panel.
- **Achievements via s&box Services** (client-side Stats + Achievements), driven by
  server-pushed `you.*` deltas, deduped by `round_id`/`game_id` (stat increments aren't
  idempotent). Initial set: `first_point`, `points_50`, `points_100`, `top_5`, `top_3`,
  `first_win`, `wins_5`, `wins_10` (total score 285 / 1000 cap). Separate from the Postgres
  hourly board.
- **HUD is ONE full-screen PanelComponent** (`client/Code/UI/Hud.razor`), not several. A
  `ScreenPanel` row-flexes its child PanelComponent roots and **ignores `position` set on a
  root**, so sibling panels can't pin/center themselves — `position:absolute` on a `<root>`
  is a no-op. The working pattern (mirrors rotaliate's `GameHud`): a single root with
  `width/height:100%` that fills the screen, with every piece an absolutely-positioned
  **child**. ScreenPanel auto-scales to a 1080-tall reference, so vertical px is aspect-stable.
  When s&box layout surprises you, read `../sbox-docs/docs/ui/` (and check rotaliate) to learn
  the model — don't assume web-CSS semantics.

---

## WebSocket protocol (summary — full in PLAN.md §8)

- Hot-path frames: the live-window `tick` (out) is **binary**; `click` (in) stays JSON
  (sent immediately, never coalesced) with additive int16 `x`/`y`. The rest JSON.
- Client→server: `click {nonce, x?, y?}`, `cursor {x, y}` (v5; ~15/s, armed-only), `test_answer {id, answer}`, `ping`.
- Server→client: `hello` (+`tick_ms`), `round_pending` (+`roster` `[{tag,username}]`, v5 only),
  `armed` (+`buttons` `[{id,nonce,x,y}]` — the initial board — to v5; the single legacy `nonce` to v4),
  `tick` (binary: `round`, `remaining`, the complete board mutations since the last tick
  `{slot, claimer_tag, t_arm, spawn?{id,nonce,x,y}}` + sampled opponent cursors `{tag,x,y}` — v5 only),
  `round_result` (with `you.points_delta`,
  `round_id`), `game_over` (with `you.placement`, `you.won`, `game_id`),
  `bounty_update` (payload-less; "re-fetch `/config` + `/bounties/previous`" on rollover),
  `too_late`/`rejected`,
  `test {state, id?, prompt?, message, until_ms?, cleared?}` (anticheat rung: `state` =
  `test`/`cooldown`/`ignored`; `cleared` dismisses it), `achievement` (`{ident}` — out-of-band
  manual unlock for an HTTP feat matched by IP: `fart`, `hackerman`).

---

## License

GamahCode License v1.2 — see `LICENSE`.
**Never** add attribution of any kind (to Claude or anyone else) in commit messages, code
comments, or any file in this repo.
