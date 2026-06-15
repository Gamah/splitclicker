# CLAUDE.md — Splitclicker Project Context

This file is the authoritative context document for Claude sessions working on this repo.
Read it fully before making any changes. The detailed design lives in **[PLAN.md](PLAN.md)** —
this file is the quick orientation; PLAN.md is the source of truth for architecture decisions.

**Status:** backend first pass implemented in `server/` (engine state machine, WS hub,
Facepunch auth, hourly board, Docker/Caddy) and unit-tested but not yet run end-to-end.
The s&box client in `client/` is scaffolded (auth, WS, controller, Razor UI).
**Next step: the s&box client** — build it out into a runnable game: create the in-editor
startup scene (`client/scenes/main.scene`) wiring `ClickController` + a `ScreenPanel`
hosting the panels, set `HttpAllowList`/`ApiClient.BaseUrl`, define the s&box Services
achievements/stats, and play-test against a running backend (PLAN §7).

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
    session/           # public player tag + username validation
    game/              # round/game state machine: arm RNG, race, scoring (first N by arrival)
    store/             # Postgres-backed hourly board + players (pgx)
    ws/                # WS hub: registry, single precomputed broadcast, click ingestion
    api/               # REST: /auth, /leaderboard/hourly, /health, + /ws upgrade
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
  connection: hold back its next `armed` frame `+10ms` per idle click (escalating, capped
  ~150–200ms, reset each round). Mashing becomes self-defeating. Idle clicks still rate-limited
  so this doesn't reintroduce idle traffic.
- **Traffic minimization.** Persistent WS, never polling. Idle is silent. Cheap idle conns
  (epoll lib), infrequent/long heartbeats, one precomputed broadcast on arm, race closes the
  instant click N lands (losing clicks read-and-dropped), leaderboard pushed inside
  `round_result` (never a fan-in `GET` stampede), jittered reconnect backoff.
- **Achievements via s&box Services** (client-side Stats + Achievements), driven by
  server-pushed `you.*` deltas, deduped by `round_id`/`game_id` (stat increments aren't
  idempotent). Initial set: `first_point`, `points_50`, `points_100`, `top_5`, `top_3`,
  `first_win`, `wins_5`, `wins_10` (total score 285 / 1000 cap). Separate from the Postgres
  hourly board.

---

## WebSocket protocol (summary — full in PLAN.md §8)

- Hot-path frames (`armed` out, `click` in) should be **binary** at scale; the rest JSON.
- Client→server: `click {seq}`, `ping`.
- Server→client: `hello`, `round_pending`, `armed`, `round_result` (with `you.points_delta`,
  `round_id`), `game_over` (with `you.placement`, `you.won`, `game_id`), `too_late`/`rejected`.

---

## License

GamahCode License v1.2 — see `LICENSE`.
**Never** add attribution of any kind (to Claude or anyone else) in commit messages, code
comments, or any file in this repo.
