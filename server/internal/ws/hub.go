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

	// cursorMinGap throttles inbound cursor frames per connection (~25/s ceiling; the
	// client sends ~15/s). Bounds cursor traffic without a shared rate bucket.
	cursorMinGap = 40 * time.Millisecond

	// minTickVersion is the oldest client API version that understands the
	// live-window `tick` frame, the round_pending roster, and click x/y positions
	// (all the v5 wire). Below it, a client is still a normal respected connection
	// (≥ the live floor) — it just isn't sent ticks/roster and its clicks carry no
	// position. Mirrors the old minTest/minSanction capability gates.
	//
	// Floor note: the live floor is v4, so every non-legacy connection is already
	// sanction-capable (≥ v4). The former minTestVersion(3)/minSanctionVersion(4)
	// split is therefore dead — TestCapable/SanctionCapable now collapse to
	// !Legacy. When the floor next moves to v5, this gate collapses the same way.
	minTickVersion = 5

	// outdatedNote replaces the dev note for outdated (below-live) clients, telling
	// them to update — shown alongside the troll leaderboards.
	outdatedNote = "Your client is out of date — restart s&box to update."
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

// outMsg is one queued outbound frame: its bytes plus whether to write it as a
// WebSocket binary message. Almost everything is JSON text; only the live-window
// `tick` frame is binary (see tick.go).
type outMsg struct {
	data   []byte
	binary bool
}

// Client is one connected player.
type Client struct {
	hub      *Hub
	conn     *websocket.Conn
	send     chan outMsg
	SteamID  string
	Tag      string
	Username string
	IP       string // client address (X-Forwarded-For hop), for IP-matched troll achievements

	// Legacy marks a connection from an OUTDATED client build (version below the
	// configured live version). It still rides the normal broadcast loop (so its UI
	// behaves), but the hub ignores its clicks, leaves it out of the live player
	// count, and swaps its leaderboards for the "UPDATE UPDATE / 67" troll board
	// nudging a restart. Set by the api layer from the connect path's version.
	Legacy bool

	// Version is the client's API version (from the /ws/{ver} path; bare /ws ⇒ 1).
	// Drives TickCapable — only minTickVersion+ clients receive the live-window
	// tick/roster wire and have their click positions honoured.
	Version int

	smu        sync.Mutex // guards sendClosed + the send channel
	sendClosed bool

	// Cursor is this connection's last reported pointer position (normalized int16,
	// 0 = centre), sampled into tick frames so others can render the roaming cursor.
	// Written from readPump (this conn's goroutine), read from Hub.Tick (the engine
	// goroutine), so guarded by curMu. hasCur is false until the first cursor arrives
	// or after a clear (round/game end, via Hub.Pending). lastCur throttles the inbound
	// rate; touched only in readPump, so it needs no lock.
	curMu   sync.Mutex
	curX    int16
	curY    int16
	hasCur  bool
	lastCur time.Time
}

// setCursor records this connection's latest pointer position (from readPump).
func (c *Client) setCursor(x, y int16) {
	c.curMu.Lock()
	c.curX, c.curY, c.hasCur = x, y, true
	c.curMu.Unlock()
}

// cursor returns the last reported position and whether one is set (from Hub.Tick).
func (c *Client) cursor() (int16, int16, bool) {
	c.curMu.Lock()
	defer c.curMu.Unlock()
	return c.curX, c.curY, c.hasCur
}

// clearCursor drops the stored cursor so a stale position from a finished round
// isn't sampled into the next window (called from Hub.Pending at the arming stage).
func (c *Client) clearCursor() {
	c.curMu.Lock()
	c.hasCur = false
	c.curMu.Unlock()
}

func NewClient(conn *websocket.Conn, steamID, tag, username, ip string, hub *Hub) *Client {
	return &Client{
		hub:      hub,
		conn:     conn,
		send:     make(chan outMsg, 32),
		SteamID:  steamID,
		Tag:      tag,
		Username: username,
		IP:       ip,
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

// ActivePlayerCount is the connected, non-legacy players who can actually race:
// the player count minus anyone in the benched set. Implements game.Broadcaster
// so the engine sizes N to the players who can score, not the benched ones.
func (h *Hub) ActivePlayerCount(benched map[string]bool) int {
	h.mu.RLock()
	n := 0
	for c := range h.clients {
		if !c.Legacy && !benched[c.SteamID] {
			n++
		}
	}
	h.mu.RUnlock()
	return n
}

// --- game.Broadcaster ---

func (h *Hub) Pending(p game.PendingFrame) {
	// Tick-capable (v5+) clients get the full roster ride-along so they can resolve
	// every pip's tag → username before the window opens; everyone else gets the
	// lean frame (older clients ignore the field anyway, but skipping it saves the
	// O(M²) roster bytes on connections that can't use them).
	base := pendingWire{T: "round_pending", Round: p.Round, Of: p.Of, Players: p.Players, Clicks: p.Clicks}
	lean := mustJSON(base)
	base.Roster = h.roster()
	full := mustJSON(base)
	for _, c := range h.clientList() {
		// New window: drop any cursor held from the last round so the first ticks of
		// this window only carry cursors players actually moved after arming.
		c.clearCursor()
		if tickCapable(c) {
			c.trySend(full)
		} else {
			c.trySend(lean)
		}
	}
}

// roster is the {tag, username} of every connected non-legacy player — the
// name-resolution map sent in round_pending so pips (which carry only a tag) can
// be labelled. Built fresh each arming stage; knowingly O(M²) at scale (accepted
// for MVP — see protocol.go / PLAN §19).
func (h *Hub) roster() []rosterEntry {
	h.mu.RLock()
	out := make([]rosterEntry, 0, len(h.clients))
	for c := range h.clients {
		if !c.Legacy {
			out = append(out, rosterEntry{Tag: c.Tag, Username: c.Username})
		}
	}
	h.mu.RUnlock()
	return out
}

// Tick fans out the live-window frame (clicks-remaining + the board mutations since
// the last tick + a sample of opponent cursors) to every tick-capable client. One
// binary marshal, reused for all — linear in players. Implements game.Broadcaster.
func (h *Hub) Tick(f game.TickFrame) {
	cursors := h.sampleCursors()
	var blob []byte
	for _, c := range h.clientList() {
		if !tickCapable(c) {
			continue
		}
		if blob == nil {
			blob = encodeTick(f, cursors) // marshal once, lazily (skip entirely if nobody's v5)
		}
		c.trySendBin(blob)
	}
}

// sampleCursors collects up to CursorSampleK tick-capable clients' current cursor
// positions for the tick frame. Cosmetic, so a simple first-K sample is fine; the
// cap keeps the cursor section bounded regardless of crowd size.
func (h *Hub) sampleCursors() []cursorSample {
	k := 0
	if h.engine != nil {
		k = h.engine.CursorSampleK()
	}
	if k <= 0 {
		return nil
	}
	out := make([]cursorSample, 0, k)
	for _, c := range h.clientList() {
		if !tickCapable(c) {
			continue
		}
		if x, y, ok := c.cursor(); ok {
			out = append(out, cursorSample{Tag: c.Tag, X: x, Y: y})
			if len(out) >= k {
				break
			}
		}
	}
	return out
}

// Armed fans out the armed frame. The unpenalised majority share one precomputed
// frame; penalised connections get theirs after a delay (the spam deterrent)
// with their own penalty_ms echoed in, so they can see they're being throttled.
func (h *Hub) Armed(a game.ArmedFrame) {
	// Two payload shapes: v5+ clients get the initial board of buttons; below-v5 clients
	// get the single persistent legacy nonce. Each shares one precomputed clean copy; a
	// penalised connection gets its own copy (its delay echoed in) after the hold.
	v5base := armedWire{T: "armed", Round: a.Round, Seq: a.Seq, Buttons: buttonsToWire(a.Buttons), Players: a.Players, Clicks: a.Clicks}
	v4base := armedWire{T: "armed", Round: a.Round, Seq: a.Seq, Nonce: strconv.FormatUint(a.Nonce, 16), Players: a.Players, Clicks: a.Clicks}
	cleanV5 := mustJSON(v5base)
	cleanV4 := mustJSON(v4base)
	for _, c := range h.clientList() {
		if a.Blocked[c.SteamID] {
			continue // benched by anticheat: withhold the nonce/buttons until they pass their test
		}
		base, clean := v4base, cleanV4
		if tickCapable(c) {
			base, clean = v5base, cleanV5
		}
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

// buttonsToWire converts the engine's initial board into the armed-frame button list
// (nonces hex-encoded like the legacy nonce). nil/empty stays nil so omitempty drops it.
func buttonsToWire(bs []game.Button) []buttonWire {
	if len(bs) == 0 {
		return nil
	}
	out := make([]buttonWire, len(bs))
	for i, b := range bs {
		out[i] = buttonWire{ID: b.SlotID, Nonce: strconv.FormatUint(b.Nonce, 16), X: b.X, Y: b.Y}
	}
	return out
}

// BroadcastBountyUpdate tells every connected client the active bounty changed, so
// it re-fetches /config + /bounties/previous (the new skin/countdown and the just-
// settled winner). Called from the bounty finalizer when a rollover happens — a
// rare event — so the fan-out cost is negligible. Not on the Broadcaster interface:
// it's driven by the finalizer loop in main, not the engine.
func (h *Hub) BroadcastBountyUpdate() {
	msg := mustJSON(bountyUpdateWire{T: "bounty_update"})
	for _, c := range h.clientList() {
		c.trySend(msg)
	}
}

// DevNote fans out the host-editable broadcast note to every client (empty
// clears it). Legacy clients receive it too — it's just a status line.
func (h *Hub) DevNote(note string) {
	msg := mustJSON(devNoteWire{T: "dev_note", Note: note})
	legacy := mustJSON(devNoteWire{T: "dev_note", Note: outdatedNote})
	for _, c := range h.clientList() {
		if c.Legacy {
			c.trySend(legacy) // outdated clients always see the "update" note
		} else {
			c.trySend(msg)
		}
	}
}

// SendTest pushes an anticheat test (or a clear) to the connection for steamID.
// Implements game.Broadcaster; called from the engine's Run goroutine.
func (h *Hub) SendTest(steamID string, f game.TestFrame) {
	h.mu.RLock()
	c := h.bySteam[steamID]
	h.mu.RUnlock()
	if c == nil {
		return
	}
	c.trySend(mustJSON(testWire{
		T: "test", State: f.State, ID: f.ID, Kind: f.Kind,
		Prompt: f.Prompt, Message: f.Message, UntilMs: f.UntilMs, Cleared: f.Cleared,
	}))
}

// TestCapable reports whether steamID's connected client understands anticheat
// tests. With the live floor at v4 every non-legacy connection qualifies, so this
// is just "connected and not legacy". Implements game.Broadcaster.
func (h *Hub) TestCapable(steamID string) bool {
	h.mu.RLock()
	c := h.bySteam[steamID]
	h.mu.RUnlock()
	return c != nil && !c.Legacy
}

// SanctionCapable reports whether steamID's connected client can render the
// cooldown/ignored countdown frames. As with TestCapable, the v4 floor means
// every non-legacy connection qualifies. Implements game.Broadcaster.
func (h *Hub) SanctionCapable(steamID string) bool {
	h.mu.RLock()
	c := h.bySteam[steamID]
	h.mu.RUnlock()
	return c != nil && !c.Legacy
}

// tickCapable reports whether c receives the v5 live-window wire (tick frames,
// the round_pending roster, honoured click positions).
func tickCapable(c *Client) bool {
	return c != nil && !c.Legacy && c.Version >= minTickVersion
}

// FireAchievement pushes a manual achievement unlock to every open connection
// from the given IP (a player may have several). It backs the out-of-band troll
// achievements — poking the backend into a 404, fumbling the admin password —
// which happen over plain HTTP, off the game socket, so we fan the unlock back to
// whatever game clients share that address. Returns the number of connections
// notified (0 when nobody at that IP has a socket open, so the feat is silent).
func (h *Hub) FireAchievement(ip, ident string) int {
	if ip == "" || ident == "" {
		return 0
	}
	msg := mustJSON(achievementWire{T: "achievement", Ident: ident})
	n := 0
	for _, c := range h.clientList() {
		if c.IP == ip {
			c.trySend(msg)
			n++
		}
	}
	return n
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
	devNote := snap.DevNote
	if c.Legacy {
		devNote = outdatedNote // outdated client: the hard-coded "update" note
	}
	c.trySend(mustJSON(helloWire{
		T:    "hello",
		You:  helloYou{Tag: c.Tag, Username: c.Username},
		Game: helloGame{
			Round: snap.Round, Of: snap.Of, Phase: snap.Phase.String(),
			Players: snap.Players, Clicks: snap.Clicks,
			ArmMin: snap.ArmMinSec, ArmMax: snap.ArmMaxSec,
			PenaltyBase: snap.PenaltyBaseMs, PenaltyStep: snap.PenaltyStepMs,
			DevNote: devNote,
			TickMs:  snap.TickMs,
		},
	}))
}

// --- per-client send (concurrency-safe vs. delayed/penalised writes) ---

func (c *Client) trySend(b []byte) { c.enqueue(outMsg{data: b}) }

// trySendBin queues a binary frame (the live-window tick). Same drop-on-full
// policy as trySend — a missed tick is recoverable (the next one re-states the
// count), a stalled hub is not.
func (c *Client) trySendBin(b []byte) { c.enqueue(outMsg{data: b, binary: true}) }

func (c *Client) enqueue(m outMsg) {
	c.smu.Lock()
	defer c.smu.Unlock()
	if c.sendClosed {
		return
	}
	select {
	case c.send <- m:
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

// inbound is the client→server message shape: a click echoing the arm nonce
// (hex), a ping, or a test_answer (echoing the test id with the player's answer).
type inbound struct {
	T      string `json:"t"`
	Nonce  string `json:"nonce"`
	ID     string `json:"id"`     // test_answer: the test token being answered
	Answer string `json:"answer"` // test_answer: the player's answer text
	// X, Y are the click's normalized on-screen position (int16 range, 0 = centre),
	// for the opponent pip. Pointers so "absent" (an older, below-v5 client) is
	// distinct from a real 0,0 — absent ⇒ the click scores but carries no position.
	X *int `json:"x"`
	Y *int `json:"y"`
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
			// Honour the click position only from tick-capable (v5+) clients; older
			// builds send none, so their scoring clicks are simply omitted from the
			// pip sample (they still score).
			var px, py int16
			hasPos := false
			if tickCapable(c) && in.X != nil && in.Y != nil {
				px, py, hasPos = clampI16(*in.X), clampI16(*in.Y), true
			}
			if c.hub.engine != nil {
				c.hub.engine.Submit(game.ClickEvent{
					SteamID:  c.SteamID,
					Tag:      c.Tag,
					Username: c.Username,
					Nonce:    nonce,
					At:       now,
					X:        px,
					Y:        py,
					HasPos:   hasPos,
				})
			}
		case "cursor":
			// Opponent-cursor position (v5+). Throttled per-connection so a flood of
			// cursor frames can't crowd out clicks (no shared WS rate bucket exists yet;
			// any future one must budget cursors separately — see PLAN). Stored, then
			// sampled into the next tick; ignored from below-v5 clients.
			if !tickCapable(c) || in.X == nil || in.Y == nil {
				continue
			}
			if now.Sub(c.lastCur) < cursorMinGap {
				continue
			}
			c.lastCur = now
			c.setCursor(clampI16(*in.X), clampI16(*in.Y))
		case "test_answer":
			if c.Legacy {
				continue // outdated client: never under test
			}
			if c.hub.engine != nil {
				c.hub.engine.SubmitAnswer(game.NewAnswer(c.SteamID, in.ID, in.Answer))
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
			mt := websocket.TextMessage
			if msg.binary {
				mt = websocket.BinaryMessage
			}
			if err := c.conn.WriteMessage(mt, msg.data); err != nil {
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

// clampI16 clamps a client-supplied click coordinate into int16 range so a
// malformed/oversized value can't overflow the binary tick encoding.
func clampI16(v int) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32767 {
		return -32767
	}
	return int16(v)
}
