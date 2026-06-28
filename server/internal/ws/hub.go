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
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
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

	// trackCap bounds one connection's per-window replay cursor path. A window is at
	// most RaceMax (~5s) and inbound cursors are throttled to ~25/s, so ~125 samples is
	// the realistic ceiling; 512 is generous headroom that still caps a stuck window.
	trackCap = 512

	// minArmingVersion is the oldest client API version that sends cursors during the
	// ARMING phase (and the `touch {id}` first-entry signal). It gates the arming-phase
	// AFK pass and the touch checks: a v6 (N-1) client sends cursors armed-only, so it
	// CANNOT be arming-AFK-judged and never sends `touch` — judging it on the arming
	// window would flag every v6 player as still. v6 still gets every other check (it
	// sends armed cursors, so the round-end `busted` !SawCursor check won't false-fire).
	// This is the new N-1 special-case the v8 bump will prune (api-bump-cleans-N-2 rule).
	minArmingVersion = 7

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

	// pendGen counts ARMING windows. Bumped in Pending (the round/window origin now that
	// cursors are captured during arming); each connected, non-parked client's pendSeen is
	// stamped to it so the afk pass can tell who was present for the WHOLE window (present
	// at the start of arming) from who joined mid-window or hasn't had a window yet. Touched
	// only from the engine Run goroutine (Pending + the cursor-activity snapshots), so it
	// needs no lock.
	pendGen int

	// windowAtNs is the current window's wall-clock ORIGIN (unix nanos): the start of the
	// arming phase, stamped in Pending. The cursor-capture path reads it to offset each
	// recorded sample from the window origin (the durable game replay's arming-origin
	// timeline) — cursors are now captured during arming, not just the armed window.
	// Atomic: written from the engine Run goroutine, read from connection readPump goroutines.
	windowAtNs atomic.Int64
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
	// force bypasses the parked withhold gate (see enqueue): used only by the park
	// notification itself, which must reach a connection at the instant it's parked.
	force bool
	// banVisible lets a frame through the shadowban withhold gate (see enqueue): set
	// only on the `hello` and `armed` frames, the sole frames a silently-banned
	// connection receives (so it looks alive and visibly sees the world arm).
	banVisible bool
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
	// Drives touchCapable (≥ minArmingVersion gets arming cursors + the touch signal);
	// the tick/park wires are gated only by !Legacy (see tickCapable/parkCapable), since
	// the live floor is at/above the version that introduced them.
	Version int

	// Shadowbanned marks a silently-banned account. Set once at connect from the DB
	// ban list (api.wsConnect), before the pumps start, so it needs no lock. A
	// shadowbanned connection still receives `hello` and `armed` (so the client looks
	// alive and the world visibly keeps arming), but EVERY other frame is withheld at
	// the enqueue choke point and ALL its inbound messages are dropped in readPump —
	// it can click forever and never score, win, or appear on a board, with nothing
	// that reveals the ban. It's left out of the round's N (ActivePlayerCount), the
	// name roster, the cursor sample, and the AFK pass, so it never generates sanctions
	// or leaks its tag to others — but it DOES count toward PlayerCount, so a lone
	// banned player still sees the world arm. Enforcement is at connect, so a ban takes
	// effect on the next connect (the admin "drop" control forces that reconnect).
	Shadowbanned bool

	smu        sync.Mutex // guards sendClosed + the send channel
	sendClosed bool

	// parked is set when this connection has stepped away: either auto-parked by the
	// engine off an afk verdict (Hub.Park) or by the player hitting Pause (a
	// client→server park{on:true}). While parked the hub withholds EVERY frame from it
	// except the forced park notification, and the connection counts toward neither the
	// crowd (PlayerCount) nor the round's N (ActivePlayerCount), nor is it judged by the
	// afk pass (cursor-activity snapshots omit it) — it's cleanly "not here" until it
	// unparks. Atomic: written from Hub.Park / Hub.applyDeferredPark (engine goroutine)
	// and readPump (this conn), read from the send path + the count/roster/cursor methods.
	parked atomic.Bool

	// parkWant/resumeWant defer a park/unpark request to the next arming (Pending)
	// boundary while the button is armed: the armed-window roster is FROZEN at arm, so a
	// scorer can't park mid-window to dodge the round-end `busted` check, and a mid-window
	// unpark can't inject an ineligible-but-playing roster member. Set from the readPump
	// `park` handler when Phase==Armed; applied (and cleared) in Hub.Pending before the
	// pendSeen stamp. Outside Armed the handler applies the toggle immediately and never
	// sets these. Atomics: written from readPump, read+cleared from the engine goroutine.
	parkWant   atomic.Bool
	resumeWant atomic.Bool

	// lastPingAt is the unix-nanos send time of the most recent server keepalive ping;
	// the pong handler reads it to compute this connection's round-trip time and fold the
	// minimum into minRTTms (the self-calibrating floor for the impossible_latency check).
	// 0 until the first ping. minRTTms is the smallest RTT seen (ms), or 0 if none yet.
	// Atomics: lastPingAt written in writePump, read in the pong handler; minRTTms written
	// in the pong handler, read by Hub.MinRTTms (engine goroutine).
	lastPingAt atomic.Int64
	minRTTms   atomic.Int64

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

	// Per-window cursor movement, for the engine's afk check. movN counts the cursor
	// samples reported since the last clearCursor (this armed window). The FIRST sample
	// is excluded: the client's first armed-frame report is taken before the board layout
	// settles, so it lands a constant offset from the rest and would make a still cursor
	// look like it moved. The anchor is the SECOND sample; moved flips true if any later
	// sample differs from it. The afk check reads "saw a cursor at all" (movN) and
	// "moved". Same curMu as the position above.
	movN       int
	movSeen    bool // anchor recorded (a second sample arrived this window)
	moved      bool // a post-anchor sample differed from the anchor
	movAnchorX int16
	movAnchorY int16

	// track accumulates this connection's cursor samples for the durable game replay:
	// every sample (ms offset from the window origin + position) reported during the window
	// (arming and armed, now that cursors are captured during arming). The full path,
	// distinct from the movement summary above. Reset at the arming stage (clearCursor) and
	// snapshotted at round end (Hub.AllCursorTracks). Guarded by curMu; bounded by trackCap
	// so a stuck window can't grow it without limit.
	track []game.CursorSample

	// touch records, per live button id, the ms-offset-from-window-origin at which this
	// connection's cursor FIRST entered that button's hitbox this window (the `touch {id}`
	// signal — sent once on the enter transition). The engine's fast_hover dwell check reads
	// it: a touch→click gap below the human floor isn't a real hover-and-press. Only v7+
	// clients send touch. Reset at the arming stage (clearCursor); guarded by curMu.
	touch map[uint16]int

	// pendSeen is the hub's pendGen at the last arming stage this client was present and
	// unparked for. Compared to the current pendGen in the cursor-activity snapshots to
	// decide afk eligibility (present at the start of the window). Touched only from the
	// engine Run goroutine (Hub.Pending).
	pendSeen int
}

// setCursor records this connection's latest pointer position (from readPump) and
// updates the per-window movement state used by the afk check. curX/curY (latest
// position, for rendering and tick sampling) update on every sample; the movement state
// skips the transient first sample and anchors on the second (see the field comment).
func (c *Client) setCursor(x, y int16) {
	c.curMu.Lock()
	c.curX, c.curY, c.hasCur = x, y, true
	c.movN++
	switch {
	case c.movN <= 1:
		// Transient first sample (pre-layout): position recorded above, ignored for movement.
	case !c.movSeen:
		c.movSeen = true
		c.movAnchorX, c.movAnchorY = x, y
	case x != c.movAnchorX || y != c.movAnchorY:
		c.moved = true
	}
	c.curMu.Unlock()
}

// recordTrack appends one cursor sample to the per-window replay path (offset ms from
// the window origin + position), capped at trackCap. Called from readPump after setCursor.
func (c *Client) recordTrack(offsetMs int, x, y int16) {
	if offsetMs < 0 {
		offsetMs = 0
	}
	c.curMu.Lock()
	if len(c.track) < trackCap {
		c.track = append(c.track, game.CursorSample{TMs: offsetMs, X: x, Y: y})
	}
	c.curMu.Unlock()
}

// recordTouch stamps the first-entry time (ms offset from the window origin) for one
// button id, ignoring repeats (only the first touch of a button this window matters).
// Called from readPump for a `touch {id}` frame.
func (c *Client) recordTouch(id uint16, offsetMs int) {
	if offsetMs < 0 {
		offsetMs = 0
	}
	c.curMu.Lock()
	if c.touch == nil {
		c.touch = map[uint16]int{}
	}
	if _, seen := c.touch[id]; !seen {
		c.touch[id] = offsetMs
	}
	c.curMu.Unlock()
}

// snapshotTouch returns a copy of this window's first-touch offsets (button id → ms).
func (c *Client) snapshotTouch() map[uint16]int {
	c.curMu.Lock()
	defer c.curMu.Unlock()
	if len(c.touch) == 0 {
		return nil
	}
	out := make(map[uint16]int, len(c.touch))
	for id, ms := range c.touch {
		out[id] = ms
	}
	return out
}

// snapshotTrack returns a copy of this window's recorded cursor path (for the replay).
func (c *Client) snapshotTrack() []game.CursorSample {
	c.curMu.Lock()
	defer c.curMu.Unlock()
	if len(c.track) == 0 {
		return nil
	}
	out := make([]game.CursorSample, len(c.track))
	copy(out, c.track)
	return out
}

// cursor returns the last reported position and whether one is set (from Hub.Tick).
func (c *Client) cursor() (int16, int16, bool) {
	c.curMu.Lock()
	defer c.curMu.Unlock()
	return c.curX, c.curY, c.hasCur
}

// movement reports whether any cursor arrived this window (movN) and whether it moved
// (a sample after the anchor differed from it). Read at round end by CursorActivity for
// the afk check.
func (c *Client) movement() (seen, moved bool) {
	c.curMu.Lock()
	defer c.curMu.Unlock()
	return c.movN >= 1, c.moved
}

// clearCursor drops the stored cursor AND resets the per-window movement state so a
// stale position from a finished round isn't carried into the next window (called from
// Hub.Pending at the arming stage).
func (c *Client) clearCursor() {
	c.curMu.Lock()
	c.hasCur = false
	c.movN = 0
	c.movSeen = false
	c.moved = false
	c.track = nil // drop the finished window's replay path
	c.touch = nil // drop the finished window's touch stamps
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

// ConnInfo is a snapshot of one live connection for the admin connections panel.
type ConnInfo struct {
	SteamID      string
	Tag          string
	Username     string
	IP           string
	Version      int
	Legacy       bool
	Parked       bool
	Shadowbanned bool
}

// Connections snapshots every live connection, sorted by SteamID for a stable
// admin display. Read-only; safe to call from the HTTP goroutine.
func (h *Hub) Connections() []ConnInfo {
	h.mu.RLock()
	out := make([]ConnInfo, 0, len(h.clients))
	for c := range h.clients {
		out = append(out, ConnInfo{
			SteamID:      c.SteamID,
			Tag:          c.Tag,
			Username:     c.Username,
			IP:           c.IP,
			Version:      c.Version,
			Legacy:       c.Legacy,
			Parked:       c.parked.Load(),
			Shadowbanned: c.Shadowbanned,
		})
	}
	h.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].SteamID < out[j].SteamID })
	return out
}

// Drop force-closes steamID's current connection so the client must reconnect.
// Closing the underlying conn makes both pumps exit; readPump's defer unregisters
// it. Returns false if no connection for that account is open. Used by the admin
// "drop" control — and, with a shadowban already saved, it's how a ban is made to
// take effect now (the reconnect re-runs the connect-time ban check).
func (h *Hub) Drop(steamID string) bool {
	h.mu.RLock()
	c := h.bySteam[steamID]
	h.mu.RUnlock()
	if c == nil {
		return false
	}
	c.conn.Close()
	return true
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
		// Outdated clients don't count toward the live crowd; parked (away) players
		// don't either, so the engine pauses if everyone present has stepped away.
		if !c.Legacy && !c.parked.Load() {
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
		// Parked players have stepped away — exclude them from N alongside the benched,
		// so a round's scoring slots are sized only to players who can actually race.
		// Shadowbanned players can't score either, so they never inflate N.
		if !c.Legacy && !c.parked.Load() && !c.Shadowbanned && !benched[c.SteamID] {
			n++
		}
	}
	h.mu.RUnlock()
	return n
}

// --- game.Broadcaster ---

func (h *Hub) Pending(p game.PendingFrame) {
	clients := h.clientList()

	// The arming stage is the round/window boundary. Apply any park/unpark requests that
	// were deferred while the previous window was armed (the frozen-roster rule), THEN
	// stamp eligibility — a player re-admitted here is stamped eligible and must put their
	// cursor on the board during this arming or they're AFK ("RESUME then sit idle" is
	// genuinely AFK).
	for _, c := range clients {
		c.applyDeferredPark()
	}

	// New window origin: cursors are now captured from the start of arming, so the replay
	// timeline and the per-window movement state both anchor here.
	h.windowAtNs.Store(time.Now().UnixNano())
	h.pendGen++
	for _, c := range clients {
		// Drop any cursor/touch held from the last window so this window's movement and
		// dwell state start clean.
		c.clearCursor()
		// Stamp present-at-arming on every connected, non-parked client (mirror the
		// parked-skip Hub.Armed used to do): a parked client gets no frames this window,
		// so leaving its pendSeen behind keeps it ineligible until it actually returns.
		if !c.parked.Load() {
			c.pendSeen = h.pendGen
		}
	}

	// Tick-capable clients get the full roster ride-along so they can resolve every pip's
	// tag → username before the window opens; everyone else gets the lean frame (older
	// clients ignore the field anyway, but skipping it saves the O(M²) roster bytes).
	base := pendingWire{T: "round_pending", Round: p.Round, Of: p.Of, Players: p.Players, Clicks: p.Clicks}
	lean := mustJSON(base)
	base.Roster = h.roster()
	full := mustJSON(base)
	for _, c := range clients {
		if tickCapable(c) {
			c.trySend(full)
		} else {
			c.trySend(lean)
		}
	}
}

// applyDeferredPark applies a park/unpark request that was deferred while the button was
// armed (see the parkWant/resumeWant fields). Called from Hub.Pending at the arming
// boundary. A resume that lands here re-admits the player to this window; a park drops
// them. Both flags are cleared. resume takes precedence if (pathologically) both are set.
func (c *Client) applyDeferredPark() {
	if c.resumeWant.Swap(false) {
		c.parkWant.Store(false)
		if c.parked.Swap(false) && c.hub.engine != nil {
			c.hub.engine.Wake()
		}
		return
	}
	if c.parkWant.Swap(false) {
		if !c.parked.Swap(true) {
			c.enqueue(outMsg{data: mustJSON(parkWire{T: "park", On: true}), force: true})
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
		// Parked players aren't in the round, so they aren't opponents — leave them out
		// of the name-resolution roster (no pips/cursors will reference them anyway).
		// Shadowbanned players aren't real opponents either, and we never leak their tag.
		if !c.Legacy && !c.parked.Load() && !c.Shadowbanned {
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
		if !tickCapable(c) || c.parked.Load() || c.Shadowbanned {
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

// AllCursorActivity snapshots cursor movement during the window just played for
// EVERY connected non-legacy player, for the engine's per-round afk pass. The afk
// check is decoupled from scoring, so it needs the whole roster (a player who sits
// still and never scores is exactly the one to catch), not just scorers. Legacy
// clients send no cursors and are omitted (the afk pass only judges who it can see,
// so they are never flagged). Called from the engine Run goroutine at round end,
// before the next arming stage clears the per-window boxes.
func (h *Hub) AllCursorActivity() map[string]game.CursorActivity {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]game.CursorActivity, len(h.clients))
	for c := range h.clients {
		// Parked players have stepped away and receive no armed frame, so they can't move
		// a cursor — omit them so the afk pass never (re)flags an away player. Their park
		// already removed them; re-flagging would be both pointless and unfair.
		// Shadowbanned players are omitted too: they receive armed but their cursors are
		// dropped (and we never want to sanction/park a silently-banned account).
		if c.Legacy || c.parked.Load() || c.Shadowbanned {
			continue
		}
		seen, moved := c.movement()
		out[c.SteamID] = game.CursorActivity{
			Tracked:   true,
			SawCursor: seen,
			Moved:     moved,
			Eligible:  c.pendSeen == h.pendGen,
		}
	}
	return out
}

// ArmingCursorActivity snapshots cursor movement for the arming-phase AFK pass, which the
// engine runs after pending() returns and before the button arms. It is the SAME movement
// state as AllCursorActivity, but restricted to clients new enough to send cursors during
// arming (>= minArmingVersion): a v6 (N-1) client sends cursors armed-only, so judging it
// on the arming window would flag it AFK every round — it is omitted here and exempt from
// the arming AFK pass (it still gets every round-end check). Parked/shadowbanned clients
// are omitted as in AllCursorActivity. Called from the engine Run goroutine.
func (h *Hub) ArmingCursorActivity() map[string]game.CursorActivity {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]game.CursorActivity, len(h.clients))
	for c := range h.clients {
		if c.Legacy || c.Version < minArmingVersion || c.parked.Load() || c.Shadowbanned {
			continue
		}
		seen, moved := c.movement()
		out[c.SteamID] = game.CursorActivity{
			Tracked:   true,
			SawCursor: seen,
			Moved:     moved,
			Eligible:  c.pendSeen == h.pendGen,
		}
	}
	return out
}

// AllTouchData snapshots every connected, touch-capable (v7+) player's first-touch offsets
// (button id → ms since the window origin) for the round just played, for the engine's
// fast_hover dwell check. Parked/shadowbanned/legacy/v6 clients are omitted (v6 sends no
// touch). Called from the engine Run goroutine at round end, before the next arming stage
// clears the per-window capture.
func (h *Hub) AllTouchData() map[string]map[uint16]int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]map[uint16]int, len(h.clients))
	for c := range h.clients {
		if c.Legacy || c.Version < minArmingVersion || c.parked.Load() || c.Shadowbanned {
			continue
		}
		// Every touch-capable player gets an entry even with no touches: PRESENCE in the
		// map means "judgeable for fast_hover". Absence means non-touch-capable (v6/legacy),
		// which the touch check exempts.
		out[c.SteamID] = c.snapshotTouch()
	}
	return out
}

// MinRTTms snapshots every connected player's minimum observed ping round-trip (ms), for
// the impossible_latency check (a scoring click faster than the player's own RTT is
// impossible). A player with no measured RTT yet is omitted (0 ⇒ "unknown", never used as
// a floor). Called from the engine Run goroutine.
func (h *Hub) MinRTTms() map[string]int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]int, len(h.clients))
	for c := range h.clients {
		if rtt := c.minRTTms.Load(); rtt > 0 {
			out[c.SteamID] = int(rtt)
		}
	}
	return out
}

// AllCursorTracks snapshots the full recorded cursor path of every connected
// non-legacy player during the window just played, for the durable game replay. Keyed
// by SteamID; players with no samples are omitted. Shadowbanned accounts are excluded
// (their cursors are dropped from the live sample too, and a silent ban must never
// surface in a replay); parked players send no cursors, so they naturally drop out.
// Called from the engine Run goroutine at round end, before the next arming stage
// clears the per-window capture.
func (h *Hub) AllCursorTracks() map[string]game.CursorTrack {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]game.CursorTrack, len(h.clients))
	for c := range h.clients {
		if c.Legacy || c.Shadowbanned {
			continue
		}
		samples := c.snapshotTrack()
		if len(samples) == 0 {
			continue
		}
		out[c.SteamID] = game.CursorTrack{Tag: c.Tag, Username: c.Username, Samples: samples}
	}
	return out
}

// Armed fans out the armed frame: a single precomputed broadcast to every non-blocked
// connection. The old per-connection delayed-arm spam penalty was gutted in v7 (the nonce
// + rate limiter + the anticheat checks cover blind flooding, and a delayed arm desynced a
// penalised player's window in a game whose whole premise is wire-arrival fairness), so
// this collapses to one frame. The eligibility stamp moved to Pending (cursors are now
// captured from the start of arming, so the window origin is the arming stage).
func (h *Hub) Armed(a game.ArmedFrame) {
	// One payload shape: the initial board of buttons. A legacy client receives it too —
	// its clicks are ignored anyway, and an old build simply doesn't render the board.
	clean := mustJSON(armedWire{T: "armed", Round: a.Round, Seq: a.Seq, Buttons: buttonsToWire(a.Buttons), Players: a.Players, Clicks: a.Clicks})
	for _, c := range h.clientList() {
		if a.Blocked[c.SteamID] {
			continue // benched by anticheat: withhold the buttons until they pass their test
		}
		// banVisible: `armed` is the one in-game frame a shadowbanned client receives,
		// so it sees the world arm (and clicks into the void).
		c.enqueue(outMsg{banVisible: true, data: clean})
	}
}

// buttonsToWire converts the engine's initial board into the armed-frame button list
// (nonces hex-encoded). nil/empty stays nil so omitempty drops it.
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
// tests. Every non-legacy connection (≥ the live floor) qualifies, so this is just
// "connected and not legacy". Implements game.Broadcaster.
func (h *Hub) TestCapable(steamID string) bool {
	h.mu.RLock()
	c := h.bySteam[steamID]
	h.mu.RUnlock()
	return c != nil && !c.Legacy
}

// SanctionCapable reports whether steamID's connected client can render the
// cooldown/ignored countdown frames. As with TestCapable, every non-legacy
// connection qualifies. Implements game.Broadcaster.
func (h *Hub) SanctionCapable(steamID string) bool {
	h.mu.RLock()
	c := h.bySteam[steamID]
	h.mu.RUnlock()
	return c != nil && !c.Legacy
}

// tickCapable reports whether c receives the live-window wire (tick frames, the
// round_pending roster, honoured click positions). With the live floor at v5 — the
// version that introduced that wire — every non-legacy connection is on it, so this is
// just "connected and not legacy". (The former v5 version gate collapsed when v5 became
// the floor, mirroring how TestCapable/SanctionCapable reduced to !Legacy.)
func tickCapable(c *Client) bool {
	return c != nil && !c.Legacy
}

// parkCapable reports whether c understands the park protocol (the server→client `park`
// frame and the client→server `park {on}` toggle). With the live floor at v6 — the version
// that introduced parking — every non-legacy connection is on it, so this collapses to
// "connected and not legacy" (mirroring how tickCapable/TestCapable already reduced when
// the floor reached v5). Only such a client may be parked — parking one that can't
// render/clear it would silently strand it.
func parkCapable(c *Client) bool {
	return c != nil && !c.Legacy
}

// touchCapable reports whether c sends arming-phase cursors and the `touch {id}` signal
// (>= minArmingVersion, i.e. v7+). Gates the arming AFK pass and the touch checks; a v6
// (N-1) client is not touch-capable and is exempt from both.
func touchCapable(c *Client) bool {
	return c != nil && !c.Legacy && c.Version >= minArmingVersion
}

// Park marks steamID's connection as away (auto-parked off an afk verdict, now raised at
// the END of arming so the player drops out BEFORE the button arms): the hub withholds
// every subsequent frame from it until the client unparks, and it drops out of the crowd
// count, the round's N, and the afk pass. Only park-capable (non-legacy) clients are
// parked — on a legacy build this is a no-op, so an afk verdict there keeps the plain
// sanction ladder. Implements game.Broadcaster; called from the engine Run goroutine. The
// client is told via a forced park frame (it bypasses the withhold gate set just before),
// then engages its Pause control and waits for the next arm to rejoin. A second Park on an
// already-parked connection is a no-op.
func (h *Hub) Park(steamID string) {
	h.mu.RLock()
	c := h.bySteam[steamID]
	h.mu.RUnlock()
	if !parkCapable(c) {
		return
	}
	if c.parked.Swap(true) {
		return // already parked
	}
	c.enqueue(outMsg{data: mustJSON(parkWire{T: "park", On: true}), force: true})
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
	// banVisible: a shadowbanned client still gets hello, so its HUD initializes and
	// the ban stays invisible (a client that never received hello would visibly fail).
	c.enqueue(outMsg{banVisible: true, data: mustJSON(helloWire{
		T:    "hello",
		You:  helloYou{Tag: c.Tag, Username: c.Username},
		Game: helloGame{
			Round: snap.Round, Of: snap.Of, Phase: snap.Phase.String(),
			Players: snap.Players, Clicks: snap.Clicks,
			ArmMin: snap.ArmMinSec, ArmMax: snap.ArmMaxSec,
			DevNote: devNote,
			TickMs:  snap.TickMs,
		},
	})})
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
	// Shadowbanned: withhold every frame except those marked banVisible (`hello` +
	// `armed`), so the client looks alive and sees the world arm but never receives a
	// result, standings, tick, or test that would let it score or reveal the ban.
	if c.Shadowbanned && !m.banVisible {
		return
	}
	// Parked: the player has stepped away, so withhold every frame until they unpark —
	// EXCEPT the forced park notification, which is what tells them they're parked. This
	// single choke point covers every broadcast path (including the delayed/penalised
	// armed writes that fire from AfterFunc timers).
	if !m.force && c.parked.Load() {
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
	// for the opponent pip. Pointers so "absent" is distinct from a real 0,0 — absent
	// ⇒ the click scores but carries no position.
	X *int `json:"x"`
	Y *int `json:"y"`
	// On is the park toggle (`park` frame): true = the player hit Pause / stepped
	// away, false = they're back. Drives the parked withhold gate.
	On bool `json:"on"`
	// Bid is the button id of a `touch` frame (v7+): the live button the cursor just
	// entered. A pointer so absent is distinct from button 0; carried under "b" rather
	// than "id" because test_answer already claims "id" as a string token.
	Bid *int `json:"b"`
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
		// Fold this round-trip into the connection's min observed RTT (the
		// impossible_latency floor). lastPingAt is the send time of the ping this pong
		// answers; 0 before the first ping. A pong arriving before the next ping is the
		// pairing, so the gap is a clean RTT sample.
		if sentNs := c.lastPingAt.Swap(0); sentNs > 0 {
			rtt := time.Since(time.Unix(0, sentNs)).Milliseconds()
			if rtt > 0 {
				if cur := c.minRTTms.Load(); cur == 0 || rtt < cur {
					c.minRTTms.Store(rtt)
				}
			}
		}
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
		if c.Shadowbanned {
			// Silently banned: drop every inbound frame (click/cursor/park/test_answer/
			// ping). We keep reading so the pong handler refreshes the read deadline and a
			// real disconnect is still detected — the socket just does nothing.
			continue
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
			// Honour the click position when present (every non-legacy client sends it);
			// a click without one still scores, just with no pip sample.
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
			// Opponent-cursor position. Throttled per-connection so a flood of cursor
			// frames can't crowd out clicks (no shared WS rate bucket exists yet; any
			// future one must budget cursors separately — see PLAN). Stored, then sampled
			// into the next tick; ignored from legacy clients.
			if !tickCapable(c) || in.X == nil || in.Y == nil {
				continue
			}
			if now.Sub(c.lastCur) < cursorMinGap {
				continue
			}
			c.lastCur = now
			cx, cy := clampI16(*in.X), clampI16(*in.Y)
			c.setCursor(cx, cy)
			// Record the sample for the durable replay too, offset from this window's origin
			// (the start of arming). windowAtNs is 0 before the first window this process —
			// skip then.
			if winNs := c.hub.windowAtNs.Load(); winNs > 0 {
				c.recordTrack(int(now.Sub(time.Unix(0, winNs)).Milliseconds()), cx, cy)
			}
		case "touch":
			// First-entry over a live button (v7+): stamp the arrival for the dwell checks.
			// Tick-agnostic like click — sent immediately, so the server gets wire-arrival
			// truth. Ignored from clients that don't send it (v6/legacy) or with no button id.
			if !touchCapable(c) || in.Bid == nil {
				continue
			}
			if winNs := c.hub.windowAtNs.Load(); winNs > 0 {
				c.recordTouch(uint16(*in.Bid), int(now.Sub(time.Unix(0, winNs)).Milliseconds()))
			}
		case "park":
			// The Pause control: on=true steps away (withhold all frames, drop out of the
			// crowd/N/afk pass), on=false rejoins. Ignored from legacy builds. While the
			// button is ARMED both directions are DEFERRED to the next arming boundary (the
			// frozen-roster rule — a scorer mustn't park mid-window to dodge `busted`, and a
			// mid-window unpark mustn't inject an ineligible-but-playing roster member);
			// outside Armed they apply immediately. Setting parked here also covers a player
			// who hits Pause BEFORE the server ever auto-parks them.
			if !parkCapable(c) {
				continue
			}
			armed := c.hub.engine != nil && c.hub.engine.Phase() == game.PhaseArmed
			if in.On {
				if armed {
					c.resumeWant.Store(false)
					c.parkWant.Store(true) // applied at the next Pending
				} else {
					c.parked.Store(true)
				}
			} else {
				if armed {
					c.parkWant.Store(false)
					c.resumeWant.Store(true) // applied at the next Pending
				} else if c.parked.Swap(false) && c.hub.engine != nil {
					c.hub.engine.Wake() // back from away: restart a paused engine
				}
			}
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
			// Stamp the send time so the matching pong yields an RTT sample (impossible_latency).
			c.lastPingAt.Store(time.Now().UnixNano())
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
