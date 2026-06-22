# Splitclicker — Design & Architecture Plan

> **Status:** Go backend (`server/`) first pass is implemented and unit-tested but not
> yet run end-to-end. The s&box client (`client/`) is scaffolded (auth, WS, controller,
> Razor UI). **Next step: build out the s&box client** — in-editor startup scene wiring
> `ClickController` + a `ScreenPanel` with the panels, `HttpAllowList`/`BaseUrl` config,
> the s&box Services achievements/stats (§7.1), and play-testing against a live backend.

## Context

**Splitclicker** is a new s&box game: a single global button that is *only clickable*
for a short burst after a random "arming" delay. When armed, the first **N** clicks
worldwide each score a point; then the button hard-disables until the next arm. A
**game** is **X rounds**; the leaderboard is shown after every round, and points
accumulate into an hourly leaderboard that resets on the clock hour.

The hard design constraint is **latency fairness**: the winner of a round is decided by
sub-frame click timing, so the click path must *not* go through the s&box engine tick.
The decided architecture (confirmed with the user) is:

- **A lightweight Go backend is the sole authority.** s&box clients connect *directly*
  to it over WebSocket and send raw click frames; the same process arms the button,
  counts clicks, and owns the leaderboard. The s&box engine is never in the click path.
- A small **arming service** (part of / co-located with the Go backend) flips the button
  to *armed* after the random delay and broadcasts it.
- **Identity = Steam**, reusing rotaliate's proven Facepunch-token validation path
  (`internal/steam.ValidateToken`). Once the token validates, the server trusts the
  client-reported SteamID. Threat model is deliberately narrow: the only realistic abuse
  is *clicking on someone else's behalf*, which is acceptable for now.

This plan reuses the rotaliate family's patterns directly: the s&box client is a thin
HTTP/WS front-end (exactly like `rotaliate-client`), and the Go server mirrors
rotaliate's WS-hub + Steam-auth + Postgres-leaderboard shape.

---

## 1. Game design

### Round / game loop (server-driven state machine)
Per global game instance the server cycles:

1. **IDLE / arming** — pick a random delay `D ∈ [armMinSec, armMaxSec]` (default 10–120s).
   Broadcast `round_pending {round, of_rounds, earliest_arm_hint?}`. Do **not** reveal D.
2. **ARMED** — at `armAt = now + D`, atomically flip to armed, stamp a server monotonic
   `armedAtNanos`, broadcast `armed {round, seq}`. Button becomes live on all clients.
3. **RACE** — accept click frames. The **first N** clicks (by server arrival order) each
   score their clicker +1 for this round. Click N closes the round.
   - Hard safety timeout `raceMaxMs` (e.g. 5s): if fewer than N clicks arrive, the round
     closes early with whatever scored.
4. **RESULT** — broadcast `round_result {round, winners[], standings[]}`; clients show the
   leaderboard for `resultDisplayMs` (e.g. 4s).
5. Next round, or after **X rounds** → `game_over {final_standings}`, brief intermission,
   then a fresh game. A new game = `round` resets to 1.

### Scoring & leaderboards
- **Round point**: +1 per scoring click. The armed window stays open until all N slots are
  consumed (or RaceMax). There is **no per-player dedupe** — a fast clicker can take several
  (or all) of the N slots in one arm, so mashing inside the live window is rewarded.
- **Game standings**: sum of round points within the current game.
- **Hourly leaderboard** (the persistent one): sum of points within the clock hour (UTC),
  resets hourly. This is the "most clicks" board the user described. Persisted to Postgres.

### Tunables (all server-side config / env)
`armMinSec=10`, `armMaxSec=120`, `clicksPerPlayer` × `connectedPlayers` = **N** (floored at
`minClicks`), `roundsPerGame X` (e.g. 10), `raceMaxMs`, `resultDisplayMs`, `intermissionMs`,
leaderboard reset cadence (default hourly). N scales with the crowd: `clicksPerPlayer=1`,
`minClicks=1` ⇒ N == connected players (`minClicks=1` & one player gives pure
first-click-wins; more players give multiple winners per arm). Player count and N are pushed
to clients (in `hello`/`round_pending`/`armed`) so the UI can show "P online · N to win".

---

## 2. Architecture overview

```
          ┌─────────────────────────── Go backend (authority) ───────────────────────────┐
 s&box    │                                                                                │
 client ──┼─ WS  /ws?ticket=…  ──►  WS hub  ──►  Game state machine (arming + race + score) │
 (thin    │                          ▲                     │                                │
  UI) ◄───┼── broadcasts ────────────┘                     ├─► Postgres (players, rounds,   │
          │                                                 │     hourly leaderboard MV)     │
          │  HTTP /api/v1/* (auth, ticket, leaderboard) ◄───┘                                │
          │  Facepunch token validation (internal/steam.ValidateToken) ───► public.facepunch │
          └────────────────────────────────────────────────────────────────────────────────┘
```

- **One authoritative Go process, one global button, one WS server.** Since every client
  talks to the backend over a persistent WebSocket and idle connections are cheap (§3.5), a
  single box comfortably holds thousands of connections — there is **no need for multiple
  "dedicated instances"**. The original "handful of dedicated instances" idea came from an
  s&box-dedicated-server framing that the WS-to-Go architecture makes unnecessary.
- **No s&box dedicated server** and **no Go sharding** for the contest. (Optional later: a
  cosmetic s&box multiplayer "room" so players see each other — out of scope here.)
- Horizontal fan-out is a *far-future scale escape hatch only* (§3.5d), not something to
  build or design around now; keep the broadcast behind a small interface so it stays
  possible, and otherwise ignore it.

### Why Go-authoritative beats engine-authoritative (recorded rationale)
The s&box engine processes networked input on its tick (`TickRate` in `.sbproj`, e.g.
50Hz → 20ms quantization) — unfair for a click race. A raw WS frame to a Go process is
decided by wire arrival order with nanosecond server stamps, no tick quantization.

---

## 3. Latency design (the core requirement)

- **Persistent WS connection per client**, established and warm *before* a round arms
  (reconnect/backoff so a player is never mid-handshake when the button arms).
- **Pre-arm priming**: on `round_pending`, the client may send nothing; on `armed` it
  sends a single tiny click frame (`{"t":"click","seq":<armSeq>}`) the instant the local
  player presses. No JSON ceremony beyond that; keep the frame minimal.
- **Server arrival order is truth.** Stamp each click with a monotonic clock
  (`time.Now()` / runtime nanotime) on read, before any locking, and order by it. Use a
  per-game mutex or a single goroutine consuming a channel of click events to serialize.
- **Anti-pre-fire via an unpredictable per-arm nonce**: each arm carries a random,
  unguessable token (`armNonce`) revealed *only* in the `armed` frame; a scoring click must
  echo the current `armNonce`. A click with a wrong/absent nonce, or that arrives while not
  ARMED, scores nothing. Because the nonce can't be known before the broadcast, a client
  **physically cannot form a valid click until it has received `armed`** — this both blocks
  blind flooding and is the prerequisite that makes the spam-penalty (§5.1) bite.
- Round closes the instant click N is accepted; late clicks get `too_late`.

---

## 3.5 Minimizing network traffic & idle cost

The whole point of persistent WS (vs. polling) is that **idle costs nothing on the wire**:
no client→server traffic and no server→client traffic flows until a round arms. Polling
thousands of clients on a timer would generate constant load *to learn nothing 95% of the
time*; we never do that. What remains to optimize is (a) idle-connection footprint,
(b) heartbeat chatter, and (c) the synchronized arm fan-out + click in-rush.

**a) Idle connections must be cheap, not absent.** Clients *must* stay connected through the
idle period because the arm time is secret and can fire at any second — disconnecting to
"save traffic" would mean missing the arm or having to leak the timing. So make idle conns
nearly free:
- Use an **epoll-based WS server** (`nbio` or `gobwas/ws`) instead of the default
  goroutine-per-connection (`gorilla/websocket` uses 2 goroutines + buffers per conn).
  This takes idle cost from ~tens of KB/conn to a few KB and removes the goroutine ceiling
  — tens of thousands of idle conns on one box. (Rotaliate uses gorilla, which is fine at
  its scale; splitclicker's fan-out scale justifies the switch.)
- No buffered writes sitting idle; allocate per-conn state lazily.

**b) Heartbeats are the silent traffic sink.** N clients × a ping every 30s = N/30 frames/s
forever. Cut it:
- **Server-driven, infrequent ping/pong** (e.g. 60–120s) instead of every client self-
  pinging; or rely on **TCP keepalive** + a generous read deadline and skip app-level pings
  entirely. Idle traffic → ~zero.
- The rotaliate client pings every 30s (`WsClient.PingInterval`); lengthen it here.

**c) The arm fan-out and click in-rush are the only real spikes — bound them.**
- **One precomputed frame, broadcast once.** Serialize the `armed` frame a single time and
  write the same bytes to every connection (don't re-marshal per client). Use a **binary**
  hot-path frame (1-byte opcode + a few bytes of seq) instead of JSON — at 10k+ clients the
  byte savings and marshal savings are real. The `armed` and `click` frames are the only
  latency/scale-critical ones; everything else can stay JSON.
- **Close the race the instant click N lands** (often N=1). The server stops counting and
  can stop *reading* further click frames for that round, so the thundering in-rush of
  losing clicks is read-and-dropped (or refused) cheaply rather than processed. Most clicks
  never need a per-click reply — send `round_result` once, not a `too_late` to everyone.
- **No per-second countdown broadcasts.** Between rounds the server sends at most one tiny
  `round_pending`; it never streams a ticking timer. Show "arming…" (indeterminate) or let
  the client animate locally — the exact arm time is secret anyway.
- **Push the leaderboard, never fan-in fetch it.** After a round, embed top-K standings +
  the player's own rank in the `round_result` frame over the *existing* WS. If every client
  instead did `GET /leaderboard` after each round, that's a synchronized HTTP stampede —
  exactly the anti-pattern. Cap to top ~20 + self, not the whole board.

**d) Horizontal fan-out — escape hatch only, not for now.** A single WS-over-Go server
handles this game's scale; we do **not** build multiple instances. *If* one box ever exceeds
its idle-conn/fan-out budget, the future move is fan-out relays: edge nodes each hold a slice
of connections, the single arming authority sends **one** arm message per edge, edges fan out
and forward scoring clicks back; hot global state (arm nonce, click count) stays
on the authority. Keep the broadcast behind a small interface so this stays possible — that's
the entire near-term obligation; otherwise ignore sharding.

**e) Reconnect storms.** On server restart, thousands reconnect at once. Require
**jittered exponential backoff** on the client (randomized first delay) so reconnects spread
over seconds, not all land in the same tick.

> Net effect: steady-state traffic ≈ a keepalive every minute or two per client and nothing
> else; a round costs one small broadcast out + up to N scoring clicks in (the rest dropped
> at the edge); the leaderboard rides the result frame. No polling anywhere.

---

## 4. Identity & auth (reuse rotaliate's Facepunch path)

Reference implementation to copy: `rotaliate/internal/steam/auth.go` →
`ValidateToken(ctx, steamID64, token)`:
- POSTs `{steamid, token}` to `https://public.facepunch.com/sbox/auth/token`,
  requires `Status=="ok"` **and** echoed `SteamId == steamID`, 5s timeout, **fails closed**.
- Client mints the token with `Sandbox.Services.Auth.GetToken("splitclicker")`
  (see `rotaliate-client/.../Api/ApiClient.cs` `AuthToken()` — service name is for client
  org only; Facepunch validates `{steamid, token}` regardless).

Flow for splitclicker:
1. Client sends `{steam_id, token, display_name}` to `POST /api/v1/auth` (or folds it into
   the WS-ticket mint). Server validates via `ValidateToken`; on success it upserts the
   player (`steam_id` unique) and returns a player record + display name.
2. Display name: the client reports the Steam display name (sanitized server-side). **There
   is no client-supplied/claimable username** (removed in #28): identity is purely the Steam
   account, the board name is the Steam display name, and any inbound `username` is ignored.
   The `players.username` column is retained nullable-and-ignored (door open for a future
   claim flow) but stays NULL, so the tag stays `sha256(steam_id)`.
3. **WS ticket pattern** (reuse rotaliate's `GetWsTicket` idea, `ApiClient.cs`): the GUID/
   SteamID is proven over HTTP once; the WS upgrade carries only a single-use short-TTL
   `?ticket=` so identity never rides the WS URL/logs. The Go side maps ticket→player.
4. After validation the server trusts the reported SteamID for that session (per the
   user's accepted threat model). No GUID-as-credential complexity is strictly required,
   but keeping a server-minted session token is cheap and matches rotaliate.

> Note: rotaliate also has full Steam **OpenID** web sign-in (`internal/steam/openid.go`).
> Not needed for an s&box-only client; skip unless a web client is added later.

---

## 5. Anti-cheat / rate limiting

- **Token-bucket per connection** for click frames (reuse the shape of rotaliate's solo
  move limiter: ~1/30ms sustained, small burst). Excess → drop + strike; repeated abuse →
  disconnect. WS-hub already enforces `MinMoveInterval`-style throttles in rotaliate.
- **No per-player dedupe within an arm** — a player may legitimately fill multiple of the N
  slots by clicking fast; the per-connection token bucket above is what bounds a masher.
- **Seq/state gating** (§3) defeats pre-fire and replayed click frames.
- **Server clock only** for ordering — never trust a client-sent timestamp.
- Connection cap / IP throttle on the WS upgrade endpoint.

### 5.1 Spam deterrent: penalized (delayed) arm signal
Rather than silently dropping clicks made while the button is dormant, *allow* them but make
mashing self-defeating: a connection that clicks during the IDLE/pending phase has its next
`armed` frame **held back** before it's written to that socket.

- **Penalty**: escalating per accepted idle click this pending phase — the Nth click adds
  N×5ms, so the accumulated `penaltyMs` runs 5,15,30,50,75,105… (`step·N(N+1)/2`), **reset to 0
  at each round start**, no cap. Fixed formula (`game.idlePenalty`), mirrored on the client
  (`ClickController.IdlePenaltyMs`) so the throttle counts up live; the authoritative `armed`
  `penalty_ms` overwrites the estimate. (Steep-but-uncapped punishes heavy mashers hard while a
  single fat-finger is only 5ms.)
- **Mechanism**: on arm, write the (precomputed) `armed` frame immediately to connections
  with `penaltyMs == 0`; schedule the dirty ones at `armBroadcast + penaltyMs` (bucket by
  penalty value so it's a handful of timers, not one per connection). One marshal, a few
  staggered send waves — preserves the §3.5(c) single-precompute broadcast.
- **Why it works — depends on the nonce (§3 anti-pre-fire)**: the penalty only matters
  because a valid scoring click must echo the `armNonce` revealed in `armed`. Delaying
  `armed` delays the earliest moment that client can produce a valid click. Without the
  nonce, a blind flooder would ignore the delayed signal and the penalty would be toothless.
- **Bounded traffic**: idle clicks are still rate-limited and dropped beyond the bucket
  (§3.5), so allowing them can't reintroduce an idle-traffic storm; honest clients send
  nothing and incur no penalty.
- **Self-consistent vs. custom clients**: a modified client could withhold idle clicks to
  dodge the penalty — but that *is* not-spamming, which is the goal. Either branch wins.
- Optional UX: surface a subtle "easy — wait for it" hint to a penalized player so the
  deterrent teaches rather than merely handicaps.

---

## 6. Go backend — components

New repo (mirror rotaliate's layout). Suggested packages:

| Package | Responsibility | Mirror in rotaliate |
|---|---|---|
| `cmd/server` | entrypoint, config from env | `cmd/server` |
| `internal/steam` | **copy `auth.go` verbatim** (`ValidateToken`) | `internal/steam` |
| `internal/game` | round/game state machine, arming RNG, scoring (first N clicks by arrival) | `internal/game`, `internal/solo` |
| `internal/ws` | WS hub: connection registry, **single precomputed broadcast**, click ingestion goroutine, rate limit, ticket lookup. Prefer an **epoll-based** lib (`nbio`/`gobwas/ws`) over goroutine-per-conn for idle-conn scale (see §3.5) | `internal/ws/hub.go` |
| `internal/api` | REST: `POST /auth`, `POST /ws/ticket`, `GET /leaderboard/*`, `GET /health` | `internal/api/router.go` |
| `internal/db` | pgx pool + goose migrations from `migrations/` (filesystem, NOT embed) | `internal/db` |
| `internal/admin` | (optional, later) stats/admin allowlisted by SteamID64 | `internal/admin` |

### Arming service
Keep it **in-process** as a goroutine driving the state machine (a separate Go binary
pushing arm events over a socket adds latency and a failure mode for no benefit now). The
"small Go app sends the arm event" idea collapses naturally into the authoritative server
itself. If multi-shard arming coordination is ever needed, promote it to its own service
then — design the state machine behind an interface so this is a later swap.

### Persistence (Postgres, goose migrations)
- `players (id uuid pk, steam_id text unique, username text, created_at)`
- `games (id, started_at, ended_at, rounds)` — completed-game history, written
  once at game end (see `00005_game_history.sql`).
- `game_rounds (id, game_id, round_no, n, players, armed_at)` — one row per round.
- `round_scores (round_id, slot_no, steam_id, offset_ms)` — one row per scoring
  click: `slot_no` is "click N", `offset_ms` its arrival latency from `armed_at`.
- `game_standings` — VIEW deriving per-game points (`COUNT(*)` of slots) + placement.
- `fastest_clickers` — MATERIALIZED VIEW of each player's mean per-round click
  delta (gap from their previous click that arm via `LAG`, first click measured
  from the arm; 10-click floor), refreshed ~every 10 min for the admin board.
  Game history is accumulated in-memory by the engine and flushed in one batched
  `RecordGame` transaction off the hot path; it never touches a WS frame.
- `hourly_scores (steam_id, hour_bucket, points)` — the authoritative per-hour tally,
  upserted on each scoring click; PK `(steam_id, hour_bucket)`.
- **Leaderboard read**: `SELECT … FROM hourly_scores WHERE hour_bucket = $current ORDER BY
  points DESC LIMIT 100`. (Materialized view optional; a hot indexed query is fine at this
  size — start simple, add an MV like rotaliate's only if needed.)
- Public payloads expose a **player_tag**-style id + username, **and** the public
  SteamID64 (`steam_id`). Decision taken: given the lighter threat model and that a
  SteamID64 is itself the public Steam-profile identifier, boards/standings carry it
  so the client can copy a player's `steamcommunity.com/profiles/{id}` link on a name
  click. (rotaliate's stricter "never expose SteamID" posture was the alternative.)

---

## 7. s&box client — components (thin, mirrors `rotaliate-client`)

Project scaffold (copy `rotaliate-client/rotaliate/` shape):
- `splitclicker.sbproj` — `GameNetworkType` can be **Singleplayer/None** (no engine
  multiplayer needed), `StartupScene` = a simple room/menu scene, **`HttpAllowList`** must
  include the backend host, `LeaderboardType: None` (we run our own).
- `Code/Api/ApiClient.cs` — copy rotaliate's: `Http.RequestAsync`, `Http.CreateJsonContent`,
  `Sandbox.Services.Auth.GetToken`, `Connection.Local.SteamId`, headers dict. Endpoints:
  `Auth()`, `GetWsTicket()`, `GetLeaderboard()`.
- `Code/Ws/WsClient.cs` — copy rotaliate's `WsClient` (Component wrapping `Sandbox.WebSocket`,
  `OnMessageReceived`, ping keepalive in `OnUpdate`). Add typed message dispatch.
- `Code/Game/PlayerData.cs` — copy the FileSystem.Data JSON identity cache (GUID/username/
  LinkedSteamId), trimmed to what's needed.
- `Code/Game/ClickController.cs` — root `Component`: owns WS lifecycle, current phase
  (pending/armed/race/result), local click capture, sends `click` frame on press.
- `Code/UI/ClickButton.razor` — `PanelComponent`; the big button. `onclick="@OnPress"`,
  disabled/enabled by phase, with juicy state styling. Capture the press with minimal work
  and fire `ClickController.SendClick()` immediately (do the WS send off the UI thread/async).
- `Code/UI/LeaderboardPanel.razor` — copy `LeaderboardScreen.razor` list pattern
  (`@foreach` over entries, `StateHasChanged()` after fetch). Shown between rounds.
- `Code/UI/StatusPanel.razor` — round counter, "arming…", countdown-to-result, your score.
- `Code/Game/Achievements.cs` — maps server-pushed `you.*` deltas to `Sandbox.Services.Stats`
  / `Achievements.Unlock` (see §7.1); dedupes by `round_id`/`game_id`.

### Input latency note
Use the UI `onclick` (or an Input action) but make the handler do *nothing* but enqueue the
WS send — no allocation, no awaiting render. The button's *enabled* state is driven by the
`armed` WS message arriving, so visual enable and scoring eligibility share one source.

---

## 7.1 Achievements (s&box Services — Stats + Achievements)

s&box ships a built-in achievements/stats backend (docs: `sbox-docs/docs/services/
achievements.md`, `stats.md`), tied to the local player's Steam identity. We use it for
**lifetime vanity progression**, kept *separate* from our own Postgres hourly competition
board (which is the live, resettable contest). Two unlock modes:
- **Stat-based** — auto-unlocks when a tracked stat crosses a threshold. We feed the stats.
- **Manual** — `Sandbox.Services.Achievements.Unlock("ident")` on an event.

### Trust model
s&box Services run **client-side**, so achievements are inherently client-trusted (the engine
is on the player's machine). That's fine: achievements are cosmetic and no leaderboard
integrity rests on them — consistent with the project's lenient threat model ("only abuse is
helping someone else"). The **server** stays the source of truth and pushes each player their
own deltas (`you.*` in `round_result`/`game_over`, §8); the **client** records the stat /
unlocks. We never let achievement state feed back into scoring.

### Stats we record (client-side, from server-pushed facts)
| Stat | How | Drives |
|---|---|---|
| `points` | `Stats.Increment("points", you.points_delta)` on each `round_result` | first_point, points_50, points_100 |
| `wins`   | `Stats.Increment("wins", 1)` on `game_over` when `you.won` | first_win, wins_5, wins_10 |

> **Win** = finishing **#1** in a game's final standings (`you.placement == 1`).

### Initial achievement set (define in the s&box project; idents must match)
| Ident | Mode | Trigger | Suggested score |
|---|---|---|---|
| `first_point` | Stat | `points` ≥ 1 | 5 |
| `points_50`   | Stat | `points` ≥ 50 | 15 |
| `points_100`  | Stat | `points` ≥ 100 | 30 |
| `top_5` | Manual | `game_over` `you.placement` ≤ 5 | 20 |
| `top_3` | Manual | `game_over` `you.placement` ≤ 3 | 40 |
| `first_win` | Stat | `wins` ≥ 1 | 25 |
| `wins_5`  | Stat | `wins` ≥ 5 | 50 |
| `wins_10` | Stat | `wins` ≥ 10 | 100 |
| `aotc` | Manual | `round_result` `you.points_delta` > 5 (more than 5 scoring clicks in one round; "Ahead of the Curve") | 30 |
| `firstwin` | Manual | `game_over` `you.won` (finish #1 in a session; "Chicken Dinner") | 25 |
| `fart` | Manual (server-pushed) | hit the backend on an unknown path and got a 404 | 10 |
| `hackerman` | Manual (server-pushed) | got the password wrong on the admin login page | 10 |

Total = 360, well under the **1000** per-game cap — leaves headroom for the "more to add
later" set. (Stat-based threshold achievements are configured with Target Stat / Aggregation
`Sum` / Min 0 / Max threshold / Show Progress. Manual achievements just need the ident defined
in the s&box project — the client unlocks them via `Achievements.Unlock`.)

> **Ahead of the Curve / Chicken Dinner** are *manual* (single-event) unlocks driven straight
> from `round_result` / `game_over`; both are idempotent, so they need no `round_id`/`game_id`
> dedupe (re-unlocking is a no-op).

> **fart / hackerman** are *server-pushed*: the feats happen over plain HTTP (a 404 on an
> unknown path; a wrong admin password), off the game socket. The server matches the requester
> to any open game connection by IP — `clientIP(r)` (the X-Forwarded-For hop, same as the rate
> limiter and the WS upgrade) — and fans an `{"t":"achievement","ident":…}` frame to those
> sockets; the client just calls `Achievements.Unlock(ident)`. If nobody at that IP has a game
> client open, the feat is silent. The `achievement` frame's `Unlock` is idempotent.

### Client implementation
- A small `Code/Game/Achievements.cs` helper (or fold into `ClickController`) handles the
  two WS events:
  - `round_result` → `Stats.Increment("points", you.points_delta)` (stat-based achievements
    fire automatically); plus `if (you.points_delta > 5) Achievements.Unlock("aotc")`.
  - `game_over` → if `you.won` then `Stats.Increment("wins", 1)` and
    `Achievements.Unlock("firstwin")`; then
    `if (you.placement <= 5) Achievements.Unlock("top_5"); if (you.placement <= 3)
    Achievements.Unlock("top_3");` (`Unlock` is idempotent — re-unlocking is a no-op).
- **Increment exactly once per event.** Stat increments are *not* idempotent, so guard
  against duplicate WS deliveries / reconnect replays: track the last applied `round_id` /
  `game_id` locally (in `PlayerData` / FileSystem.Data) and skip already-applied ones. Manual
  `Unlock` calls are naturally idempotent and need no guard.
- No new backend storage is required for achievements — they live in s&box's service. (We may
  *also* keep `wins`/`points` in our Postgres for our own leaderboards; that's the existing
  data model, independent of this.)

### Later (noted, not built now)
More achievements (streaks, fastest reaction, rounds-without-spam-penalty, daily-play, etc.)
slot in under the remaining score budget; reaction-time would need the server to push a
`reaction_ms` fact and a `reaction` stat.

---

## 8. WebSocket wire protocol (JSON text frames)

> Hot-path frames (`armed` out, `click` in) should be **binary** (1-byte opcode + seq) at
> scale; the rest stay JSON for legibility. See §3.5(c). Heartbeats: prefer protocol-level
> ping/pong on a long interval (or TCP keepalive), not an app `ping` per client every 30s.

> **v5 supersedes the single-button shape below.** The live window is now a **multi-button
> board** with **opponent cursors**: `armed` carries the initial X buttons (`buttons:[{id,nonce,
> x,y}]`) to v5 (a single legacy `nonce` to v4); the binary `tick` carries `remaining` + the
> complete board mutations since the last tick (`{slot, claimer_tag, t_arm, spawn?{id,nonce,x,y}}`)
> + a sample of cursors (`{tag,x,y}`); the client sends `cursor {x,y}` (~15/s, armed-only).
> Button positions are **server-authoritative and transmitted** (each replacement's offset is
> server-RNG'd) — there is no client-derivable seed. See CLAUDE.md "Multi-button live window +
> opponent cursors (v5)" for the authoritative description.

Client → server:
- `click` — `{seq:<armSeq>}` (binary in production); the only hot-path frame; keep tiny.
- `ping` — keepalive (infrequent; see §3.5(b)).

Server → client:
- `{"t":"hello","you":{tag,username},"game":{round,of,phase,players,clicks,arm_min,arm_max}}`
  — `arm_min`/`arm_max` are the arming-window bounds in seconds (the per-round delay itself
  stays secret); the client shows the window while a round is arming.
- `{"t":"round_pending","round":k,"of":X,"players":P,"clicks":N}`
- `{"t":"armed","round":k,"seq":s,"players":P,"clicks":N,"penalty_ms":m}` — go live now.
  `penalty_ms` is this connection's own delayed-arm penalty (§5.1), surfaced so a masher can
  see the throttle; 0 for honest clients.
- `{"t":"round_result","round":k,"winners":[…],"standings":[…],"you":{"points_delta":d,"round_id":"…"}}`
  — `you.points_delta` + a unique `round_id` let the client drive the `points` stat exactly
  once (§7.1).
- `{"t":"game_over","standings":[…],"you":{"placement":p,"won":bool,"game_id":"…"}}`
  — `you.placement`/`won` drive placement + win achievements; `game_id` dedupes.
- `{"t":"too_late"}` / `{"t":"rejected","reason":…}` — per-click feedback (optional).
- `{"t":"achievement","ident":"…"}` — fire a manual achievement unlock; pushed out-of-band
  (off the round loop) when the server detects a feat over HTTP and matches the requester to
  this socket by IP (e.g. `fart`, `hackerman`). The client calls `Achievements.Unlock(ident)`.

(Mirror rotaliate's `ws/hub.go` message-type switch.) Seeds/large ints as **JSON strings**
if any are added later (rotaliate's hard-won lesson re JS precision — relevant if a web
client appears).

---

## 9. Repo & build

- Single **monorepo** `splitclicker` with `server/` (Go backend) and `client/` (s&box)
  folders and one root README. (Originally planned as two repos mirroring
  rotaliate/rotaliate-client; consolidated into one at the user's direction.) Reuse
  rotaliate's `Makefile`, `docker/` compose, Caddy TLS, goose-on-startup, `GIT_HASH`
  version-stamp conventions.
- Backend behind Caddy (TLS); the s&box client's `HttpAllowList` and WS URL point at the
  public host. **WSS** (TLS) is required for the WS too — terminate at Caddy.

---

## 10. Critical files to reference while implementing

Server pattern sources (read these, copy shape):
- `rotaliate/internal/steam/auth.go` — `ValidateToken` (copy ~verbatim).
- `rotaliate/internal/ws/hub.go` — hub, lobby, broadcast, rate-limit timestamps.
- `rotaliate/internal/api/router.go` — Go 1.22 ServeMux routing, ticket endpoint.
- `rotaliate/internal/solo/` — server-authoritative session + token-bucket move limiter.
- `rotaliate/internal/db/`, `rotaliate/migrations/` — pgx + goose-from-filesystem.

Client pattern sources:
- `rotaliate-client/rotaliate/Code/Api/ApiClient.cs` — HTTP, auth token, headers, ws ticket.
- `rotaliate-client/rotaliate/Code/Ws/WsClient.cs` — WebSocket component.
- `rotaliate-client/rotaliate/Code/Game/PlayerData.cs` — identity persistence.
- `rotaliate-client/rotaliate/Code/UI/Screens/LeaderboardScreen.razor` — list UI + fetch.
- `rotaliate-client/rotaliate/Code/UI/Screens/ModePickerScreen.razor` — clickable buttons.
- `rotaliate-client/rotaliate/rotaliate.sbproj` — project metadata fields.

---

## 11. Open tunables / decisions deferred (not blocking)
- Exact N, X, arm range, display timings — config; tune by feel.
- Whether to keep full round/game history tables or only the hourly tally.
- player_tag privacy layer (copy rotaliate) vs. exposing SteamID — lighter threat model
  makes it optional.
- Multi-shard arming coordination — single instance first.
- Optional cosmetic s&box multiplayer room so players see each other (separate effort).

---

## 12. Verification

This is a greenfield design plan; "verification" = how we'd prove the build once implemented:
1. **Backend unit tests**: state machine (arming RNG bounds, N-click cutoff, multi-slot
   single-player scoring, seq gating, late-click rejection); `ValidateToken` (copy
   rotaliate's `auth_test.go` stub).
2. **Latency/ordering test**: fire M concurrent click goroutines at an armed round; assert
   exactly N score and ordering matches server arrival stamps.
3. **Integration**: run Go backend locally, connect 2+ s&box client instances, confirm the
   button enables on `armed`, first-N scoring, between-round leaderboard, hourly reset.
4. **Abuse test**: pre-fire clicks (wrong seq / not armed) score nothing; rate-limit trips
   on spam; one SteamID can't take multiple of the N slots in a round.
5. **Auth test**: a bad/expired Facepunch token is rejected (fail-closed); a valid token
   for a different SteamID is rejected.
6. **Achievements**: score a point → `first_point` unlocks; cross 50/100 lifetime points →
   `points_50`/`points_100`; win a game → `wins` increments and `first_win` unlocks; finish
   ≤5 / ≤3 → `top_5`/`top_3`; reconnect mid-game does **not** double-count `points`/`wins`
   (round_id/game_id dedupe). Confirm total achievement score ≤ 1000.
