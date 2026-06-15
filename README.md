# splitclicker

A competitive, global, real-time **clicker race**. One global button arms after
a secret delay; the first **N** clicks worldwide each score a point; **X** rounds
make a game; points accumulate into a per-UTC-hour leaderboard. The click path
goes straight to a Go backend over WebSocket (never through the s&box engine
tick), so rounds are decided by true server wire-arrival order.

See **[PLAN.md](PLAN.md)** for the full design and **[CLAUDE.md](CLAUDE.md)** for
orientation.

## Repo layout

This is a monorepo with two halves:

```
server/   Go backend — the sole authority (game loop, WS hub, auth, Postgres)
client/   s&box game — thin HTTP/WS front-end (C# + Razor UI)
```

### `server/` — Go backend

```
cmd/server/        entrypoint + graceful shutdown
internal/
  steam/           Facepunch auth-token validation (the only auth path)
  session/         public player tag + username validation
  game/            authoritative state machine (arm RNG, nonce race, scoring,
                   spam penalty) — transport/DB-agnostic behind interfaces
  store/           Postgres-backed hourly board + players (pgx)
  ws/              WebSocket hub (gorilla); implements game.Broadcaster
  api/             REST: /auth, /leaderboard/hourly, /health, + /ws upgrade
  db/              pgx pool + goose migrations
migrations/        goose SQL
docker/            Dockerfile, compose (app + postgres), Caddyfile (TLS/WSS)
```

Run it (from `server/`):

```sh
cp .env.example .env      # set DATABASE_URL
make migrate-up           # apply schema
make dev                  # go run ./cmd/server   (listens on :6969)
make test                 # go test ./... -race
# or the whole stack in Docker:
make up                   # app + Postgres
```

Dependencies: stdlib plus the rotaliate-family set — `gorilla/websocket`,
`jackc/pgx/v5`, `pressly/goose/v3`, `google/uuid`, `go.uber.org/zap`.

### `client/` — s&box game

Thin front-end: prove a Steam identity once (Facepunch token → WS ticket),
connect the socket, and play. `ClickController` owns the WS lifecycle and phase;
the button enables only on `armed` and a press sends `{"t":"click","nonce":…}`.
A startup scene and the s&box Services achievement/stat config are created
in-editor — see `client/`'s code comments and PLAN §7.

## Contract (HTTP / WebSocket)

1. `POST /api/v1/auth` `{steam_id, token, username?}` — client mints `token` with
   `Sandbox.Services.Auth.GetToken("splitclicker")`; server validates against
   Facepunch (fail-closed), upserts the player, returns `{tag, username, ticket, ttl_ms}`.
2. `GET /ws?ticket=…` — upgrade with the single-use ticket (SteamID never on the URL).
3. WS frames (JSON): client→ `click {nonce}`, `ping`; server→ `hello`,
   `round_pending`, `armed {nonce}`, `round_result` (with `you.points_delta`,
   `round_id`), `game_over` (with `you.placement`, `you.won`, `game_id`).
4. `GET /api/v1/leaderboard/hourly?limit=100` — current UTC hour, top players.

## Deployment / security note

The app and Postgres publish their ports **bound to `127.0.0.1` only**. Docker's
port publishing inserts iptables rules that bypass UFW, so a bare `6969:6969`
would expose the service to the internet even behind `ufw deny`. Caddy terminates
TLS on the host and reverse-proxies to the loopback port (WSS required for `/ws`).
