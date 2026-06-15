# splitclicker (backend)

The Go backend for **splitclicker** — a global, real-time clicker race. One
global button arms after a secret delay; the first **N** clicks worldwide score
a point; **X** rounds make a game; points accumulate into a per-UTC-hour
leaderboard. See **[PLAN.md](PLAN.md)** for the full design and **[CLAUDE.md](CLAUDE.md)**
for orientation.

This repo is the **server only**. The s&box client lives in a separate
`splitclicker-client` repo (not yet created).

## Layout

```
cmd/server/        entrypoint + graceful shutdown
internal/
  steam/           Facepunch auth-token validation (the only auth path)
  session/         public player tag + username validation
  game/            authoritative round/game state machine (arm RNG, race,
                   nonce, spam penalty, scoring) — transport/DB-agnostic
  store/           Postgres-backed hourly board + players (pgx)
  ws/              WebSocket hub (gorilla); implements game.Broadcaster
  api/             REST: /auth, /leaderboard/hourly, /health, + /ws upgrade
  db/              pgx pool + goose migrations
migrations/        goose SQL
docker/            Dockerfile, compose (app + postgres), Caddyfile (TLS/WSS)
```

The engine knows nothing about WebSockets or SQL — it talks to a
`game.Broadcaster` (the hub) and a `game.Store` (Postgres). That keeps it
unit-testable and keeps the broadcast swappable (the documented horizontal
fan-out escape hatch).

## Dependencies

Standard library plus the proven rotaliate-family set: `gorilla/websocket`,
`jackc/pgx/v5`, `pressly/goose/v3`, `google/uuid`, `go.uber.org/zap`.

## Run locally

```sh
cp .env.example .env          # set DATABASE_URL
make migrate-up               # apply schema (needs goose + DATABASE_URL)
make dev                      # go run ./cmd/server
make test                     # go test ./... -race
```

Or the whole stack (app + Postgres) in Docker:

```sh
make up      # build + start
make logs
make down
```

## HTTP / WebSocket contract

1. `POST /api/v1/auth` `{steam_id, token, username?}` — the client mints `token`
   with `Sandbox.Services.Auth.GetToken("splitclicker")`. The server validates it
   against Facepunch (fail-closed), upserts the player, and returns
   `{tag, username, ticket, ttl_ms}`.
2. `GET /ws?ticket=…` — upgrade with the single-use ticket. The SteamID never
   rides the URL.
3. WebSocket frames (JSON):
   - **client→server:** `{"t":"click","nonce":"<hex>"}` (echo the current arm
     nonce), `{"t":"ping"}`
   - **server→client:** `hello`, `round_pending`, `armed` (carries `nonce`),
     `round_result` (with `you.points_delta` + `round_id`), `game_over` (with
     `you.placement`, `you.won`, `game_id`).
4. `GET /api/v1/leaderboard/hourly?limit=100` — current UTC hour, top players.

## Notes / first-pass scope

- Auth is **Facepunch token validation only** — no Steam OpenID web sign-in.
- The hot-path `armed`/`click` frames are JSON here; PLAN §3.5c suggests a binary
  encoding at scale. Easy follow-up.
- Idle-click spam penalty currently accrues during the *pending* phase only
  (the bulk of mashing); result/intermission clicks are dropped without penalty.
- gorilla/websocket (goroutine-per-conn) is fine at launch scale; swap for an
  epoll reader behind the same `game.Broadcaster` interface if idle-conn cost
  ever dominates.
