// Package ws is the WebSocket hub: it holds every connected client, fans out the
// engine's broadcasts, and feeds click frames into the engine. There is one
// global button, so there is one global client set — no rooms.
//
// The hub implements game.Broadcaster. The engine calls those methods from its
// single Run goroutine; the hub fans out to per-client buffered send channels
// (goroutine-safe), so engine and connection goroutines never block each other.
//
// Scale note: this uses gorilla/websocket (goroutine-per-conn), which is fine at
// launch scale. If idle-connection cost ever dominates, swap this package for an
// epoll-based reader (nbio/gobwas) — the engine only knows the Broadcaster
// interface, so nothing else changes.
package ws

import (
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/gamah/splitclicker/internal/game"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 90 * time.Second // generous: idle conns just wait for an arm
	pingPeriod     = 60 * time.Second // server-driven keepalive (PLAN §3.5b)
	maxMessageSize = 1024
)

// compile-time check that Hub satisfies the engine's broadcast interface.
var _ game.Broadcaster = (*Hub)(nil)

// Hub owns the global client set.
type Hub struct {
	mu      sync.RWMutex
	clients map[*Client]struct{}
	bySteam map[string]*Client // newest connection per Steam account
	engine  *game.Engine
	log     *zap.Logger
}

func NewHub(log *zap.Logger) *Hub {
	if log == nil {
		log = zap.NewNop()
	}
	return &Hub{
		clients: make(map[*Client]struct{}),
		bySteam: make(map[string]*Client),
		log:     log,
	}
}

// SetEngine wires the engine in. Call once before serving any client (the hub
// and engine reference each other, so one must be constructed first).
func (h *Hub) SetEngine(e *game.Engine) { h.engine = e }

// Client is one connected player.
type Client struct {
	hub      *Hub
	conn     *websocket.Conn
	send     chan []byte
	SteamID  string
	Tag      string
	Username string

	// Legacy marks a connection from an OUTDATED client build. It still rides the
	// normal broadcast loop (so its UI behaves), but the hub ignores its clicks,
	// leaves it out of the live player count, and swaps its leaderboards for the
	// "UPDATE UPDATE / 67" troll board nudging a restart. See wsConnectLegacy.
	Legacy bool

	smu        sync.Mutex // guards sendClosed + the send channel
	sendClosed bool
}

func NewClient(conn *websocket.Conn, steamID, tag, username string, hub *Hub) *Client {
	return &Client{
		hub:      hub,
		conn:     conn,
		send:     make(chan []byte, 32),
		SteamID:  steamID,
		Tag:      tag,
		Username: username,
	}
}

// ServeClient registers the client, then runs its pumps. Blocks until disconnect.
func (h *Hub) ServeClient(c *Client) {
	h.register(c)
	go c.writePump()
	c.readPump()
}

func (h *Hub) register(c *Client) {
	h.mu.Lock()
	if old, ok := h.bySteam[c.SteamID]; ok && old != c {
		// Reconnect: evict the stale connection for this account.
		delete(h.clients, old)
		old.closeSend()
	}
	h.clients[c] = struct{}{}
	h.bySteam[c.SteamID] = c
	h.mu.Unlock()

	// Wake a paused engine so it starts a game now that someone's here. Legacy
	// clients don't count toward the crowd, so they don't wake it.
	if !c.Legacy && h.engine != nil {
		h.engine.Wake()
	}

	h.hello(c)
}

func (h *Hub) unregister(c *Client) {
	h.mu.Lock()
	delete(h.clients, c)
	if h.bySteam[c.SteamID] == c {
		delete(h.bySteam, c.SteamID)
	}
	h.mu.Unlock()
	c.closeSend()
}

func (h *Hub) clientList() []*Client {
	h.mu.RLock()
	out := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		out = append(out, c)
	}
	h.mu.RUnlock()
	return out
}

// PlayerCount is the number of open client connections. Implements
// game.Broadcaster so the engine can size each round's N to the crowd.
func (h *Hub) PlayerCount() int {
	h.mu.RLock()
	n := 0
	for c := range h.clients {
		if !c.Legacy { // outdated clients don't count toward the live crowd
			n++
		}
	}
	h.mu.RUnlock()
	return n
}

// --- game.Broadcaster ---

func (h *Hub) Pending(p game.PendingFrame) {
	msg := mustJSON(pendingWire{T: "round_pending", Round: p.Round, Of: p.Of, Players: p.Players, Clicks: p.Clicks})
	for _, c := range h.clientList() {
		c.trySend(msg)
	}
}

// Armed fans out the armed frame. The unpenalised majority share one precomputed
// frame; penalised connections get theirs after a delay (the spam deterrent)
// with their own penalty_ms echoed in, so they can see they're being throttled.
func (h *Hub) Armed(a game.ArmedFrame) {
	base := armedWire{T: "armed", Round: a.Round, Seq: a.Seq, Nonce: strconv.FormatUint(a.Nonce, 16), Players: a.Players, Clicks: a.Clicks}
	clean := mustJSON(base)
	for _, c := range h.clientList() {
		if d := a.Penalties[c.SteamID]; d > 0 {
			w := base
			w.PenaltyMs = int(d.Milliseconds())
			msg := mustJSON(w)
			cc := c
			time.AfterFunc(d, func() { cc.trySend(msg) })
		} else {
			c.trySend(clean)
		}
	}
}

// DevNote fans out the host-editable broadcast note to every client (empty
// clears it). Legacy clients receive it too — it's just a status line.
func (h *Hub) DevNote(note string) {
	msg := mustJSON(devNoteWire{T: "dev_note", Note: note})
	for _, c := range h.clientList() {
		c.trySend(msg)
	}
}

func (h *Hub) Result(r game.ResultFrame) {
	troll := LegacyBoard()
	for _, c := range h.clientList() {
		winners, standings, delta := r.Winners, r.Standings, r.Deltas[c.SteamID]
		if c.Legacy { // outdated client: feed it the troll board, never a real delta
			winners, standings, delta = troll, troll, 0
		}
		c.trySend(mustJSON(resultWire{
			T:         "round_result",
			Round:     r.Round,
			Of:        r.Of,
			Winners:   winners,
			Standings: standings,
			You:       youResult{PointsDelta: delta, RoundID: r.RoundID},
		}))
	}
}

func (h *Hub) GameOver(g game.GameOverFrame) {
	troll := LegacyBoard()
	for _, c := range h.clientList() {
		standings := g.Standings
		you := youGameOver{
			Placement:   g.Placements[c.SteamID],
			Won:         g.Won[c.SteamID],
			GameID:      g.GameID,
			PointsDelta: g.Deltas[c.SteamID],
			RoundID:     g.RoundID,
		}
		if c.Legacy { // outdated client: troll board, never placed/won/scored
			standings, you = troll, youGameOver{}
		}
		c.trySend(mustJSON(gameOverWire{T: "game_over", Standings: standings, You: you}))
	}
}

// LegacyBoard is the troll leaderboard shown to outdated (v1) clients: 15 rows of
// "UPDATE UPDATE" / 67, nudging the player to fully restart s&box to pick up the
// new build. Used both for their round_result/game_over standings (above) and by
// the v1 HTTP leaderboard endpoints (package api).
func LegacyBoard() []game.Standing {
	out := make([]game.Standing, 15)
	for i := range out {
		out[i] = game.Standing{Username: "UPDATE UPDATE", Points: 67}
	}
	return out
}

func (h *Hub) hello(c *Client) {
	var snap game.Snapshot
	if h.engine != nil {
		snap = h.engine.Snapshot()
	}
	c.trySend(mustJSON(helloWire{
		T:    "hello",
		You:  helloYou{Tag: c.Tag, Username: c.Username},
		Game: helloGame{
			Round: snap.Round, Of: snap.Of, Phase: snap.Phase.String(),
			Players: snap.Players, Clicks: snap.Clicks,
			ArmMin: snap.ArmMinSec, ArmMax: snap.ArmMaxSec,
			PenaltyBase: snap.PenaltyBaseMs, PenaltyStep: snap.PenaltyStepMs,
			DevNote: snap.DevNote,
		},
	}))
}

// --- per-client send (concurrency-safe vs. delayed/penalised writes) ---

func (c *Client) trySend(b []byte) {
	c.smu.Lock()
	defer c.smu.Unlock()
	if c.sendClosed {
		return
	}
	select {
	case c.send <- b:
	default:
		// Slow client: drop. A missed frame is recoverable; a stalled hub is not.
		c.hub.log.Warn("slow client, dropping frame", zap.String("steam_id", c.SteamID))
	}
}

func (c *Client) closeSend() {
	c.smu.Lock()
	defer c.smu.Unlock()
	if !c.sendClosed {
		c.sendClosed = true
		close(c.send)
	}
}

// --- read / write pumps ---

// inbound is the only client→server message shape: a click echoing the arm
// nonce (hex), or a ping.
type inbound struct {
	T     string `json:"t"`
	Nonce string `json:"nonce"`
}

func (c *Client) readPump() {
	defer func() {
		c.hub.unregister(c)
		c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	for {
		_, raw, err := c.conn.ReadMessage()
		// Stamp arrival the instant we have the bytes, before any parsing — this
		// timestamp (and the channel FIFO order behind it) is what decides the race.
		now := time.Now()
		if err != nil {
			return
		}
		var in inbound
		if err := json.Unmarshal(raw, &in); err != nil {
			continue
		}
		switch in.T {
		case "click":
			if c.Legacy {
				continue // outdated client: its clicks never reach the game
			}
			nonce, _ := strconv.ParseUint(in.Nonce, 16, 64) // bad/empty → 0, scores nothing
			if c.hub.engine != nil {
				c.hub.engine.Submit(game.ClickEvent{
					SteamID:  c.SteamID,
					Tag:      c.Tag,
					Username: c.Username,
					Nonce:    nonce,
					At:       now,
				})
			}
		case "ping":
			// Keepalive handled at the protocol level (SetPongHandler); nothing to do.
		}
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
