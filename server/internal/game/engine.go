// Package game is the authoritative round/game state machine: it arms the global
// button after a random delay, races the first N clicks worldwide, scores them,
// and drives the leaderboard. It owns all game state in a single goroutine
// (Run), so the hot path needs no locks: clicks arrive on a channel and are
// serialized by arrival order — the server's wire-arrival order is truth.
//
// The state machine knows nothing about WebSockets or SQL. Transport is a
// Broadcaster (the ws hub) and persistence is a Store (the hourly board); both
// are interfaces so the engine stays unit-testable and the broadcast stays
// swappable (the documented horizontal-fan-out escape hatch).
package game

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Phase is the current point in the round cycle.
type Phase int

const (
	PhaseIntermission Phase = iota // between games
	PhasePending                   // arming: waiting out the secret delay
	PhaseArmed                     // live: the race is open
	PhaseResult                    // showing the round leaderboard
)

func (p Phase) String() string {
	switch p {
	case PhasePending:
		return "pending"
	case PhaseArmed:
		return "armed"
	case PhaseResult:
		return "result"
	default:
		return "intermission"
	}
}

// ClickEvent is one click as the hub read it off the wire. At is stamped on read,
// before any locking, so ordering reflects true arrival. Nonce must echo the
// current arm's nonce to score (anti-pre-fire); 0 means "no/!valid nonce".
type ClickEvent struct {
	SteamID  string
	Tag      string
	Username string
	Nonce    uint64
	At       time.Time

	// X, Y are the clicker's on-screen click position, normalized to int16 range
	// (−32767..32767 per axis, 0 = centre), used to place the opponent pip on other
	// players' screens. HasPos is false when the client sent no position: such clicks
	// still score but are omitted from the pip sample.
	X, Y   int16
	HasPos bool
}

// Standing is one player's position on a board. SteamID64 is public information
// (it is literally the public Steam-profile identifier), so it is sent to clients
// — the UI uses it to open/copy a player's steamcommunity.com profile.
type Standing struct {
	Tag      string `json:"tag"`
	Username string `json:"username"`
	Points   int    `json:"points"`
	SteamID  string `json:"steam_id"`
	// Status is the player's live anticheat rung for the active bounty —
	// "live" / "cooldown" / "ignored" — for the leaderboard status dot.
	Status string `json:"status"`
	// BehindMs is how many milliseconds this player's tie-deciding click (the one
	// that reached their total) arrived AFTER the player ranked immediately above
	// them with the SAME points — i.e. "how much they lost the game by". Stamped
	// only on the FINAL game standings (game_over), not the per-round running
	// standings, since the tie that matters is the end-of-game one. 0 (omitted) for
	// the top of a tie group and for a unique score. Additive; older clients ignore.
	BehindMs int `json:"behind_ms,omitempty"`
}

// --- broadcaster (implemented by the ws hub) ---

// PendingFrame announces a new round is arming. The arm time is secret, so this
// carries no countdown. Players/Clicks tell the client how many people are
// connected and how many scoring clicks (N) this round will take to fill.
type PendingFrame struct {
	Round   int
	Of      int
	Players int
	Clicks  int
}

// ArmedFrame goes live: the race is open. A scoring click must echo one of Buttons'
// nonces. The old per-connection delayed-arm spam penalty was gutted in v7 (see race).
type ArmedFrame struct {
	Round int
	Seq   int
	// Buttons is the initial board of live buttons (each {SlotID, Nonce, X, Y}) sent to
	// every non-legacy client; a scoring click echoes a button's Nonce. See board.
	Buttons []Button
	Players int
	Clicks  int
	// Blocked is the set of SteamIDs withheld from this arm: players under an
	// anticheat test who haven't passed yet. The hub does not send them the armed
	// frame (no nonce ⇒ they cannot score) until they clear their test.
	Blocked map[string]bool
}

// ResultFrame is the post-round leaderboard. Winners is the round's scorers in
// first-click order with the points each took THIS round (the client flashes it);
// Standings is the cumulative game board. Deltas is per-SteamID points scored this
// round; the hub merges each connection's own delta + RoundID into its copy so the
// client can drive its `points` achievement stat exactly once.
type ResultFrame struct {
	Round     int
	Of        int
	Winners   []Standing
	Standings []Standing
	RoundID   string
	Deltas    map[string]int
}

// GameOverFrame is the final standings. Placements/Won are per-SteamID, merged
// per connection by the hub (drives placement/win achievements); GameID dedupes.
// Deltas/RoundID carry the FINAL round's points (per-SteamID) and its round id:
// the last round folds straight into game_over with no separate round_result, so
// the client still needs these here to drive its `points` stat exactly once.
type GameOverFrame struct {
	Standings  []Standing
	GameID     string
	Placements map[string]int
	Won        map[string]bool
	Deltas     map[string]int
	RoundID    string
}

// TickFrame is the coalesced live-window broadcast emitted while the button is
// armed (see Engine.race): the running clicks-remaining count plus every board
// mutation (button claimed + its replacement) since the last tick. The mutations are
// authoritative and complete (never sampled — a dropped claim would desync a client's
// board); their byte cost is O(scoring clicks), never per-non-scoring-click. The hub
// appends a bounded sample of opponent cursor positions at encode time (those live in
// the hub's connection state, not here). One precomputed frame fans out to every
// tick-capable client — linear in players. Encoded as a binary frame (ws.encodeTick).
type TickFrame struct {
	Round     int
	Remaining int
	Claims    []BoardClaim
}

// Broadcaster is how the engine reaches connected clients. All methods are
// called from the engine's single Run goroutine, except PlayerCount which must
// be safe from any goroutine (the engine reads it to size each round's race).
type Broadcaster interface {
	Pending(PendingFrame)
	Armed(ArmedFrame)
	// Tick fans out the coalesced live-window frame (count + sampled pips) to
	// tick-capable clients while the button is armed. Called at the configured
	// cadence from the race loop; a no-op fan-out is fine when nobody's tick-capable.
	Tick(TickFrame)
	Result(ResultFrame)
	GameOver(GameOverFrame)
	// DevNote pushes the current host-editable broadcast note to every client
	// (empty string clears it). Sent once per game.
	DevNote(note string)
	PlayerCount() int
	// ActivePlayerCount is the connected players who can actually race this round:
	// non-legacy and not currently benched (in the given set). Used to size N so
	// benched players don't inflate the scoring slots. Safe from any goroutine.
	ActivePlayerCount(benched map[string]bool) int
	// SendTest pushes an anticheat test (or a clear) to a single player by SteamID.
	SendTest(steamID string, f TestFrame)
	// TestCapable reports whether the SteamID's connected client understands tests
	// (a new-enough build). Only test-capable players are benched/tested; older
	// clients still have their checks run and logged. False if not connected.
	TestCapable(steamID string) bool
	// SanctionCapable reports whether the client understands the sanction states
	// (cooldown / ignored countdowns). Such a client is sidelined silently when on a
	// non-test rung — it still can't score, it just doesn't get a frame it can't
	// render. False if not connected.
	SanctionCapable(steamID string) bool
	// Park marks a player as away off an afk verdict (now raised at the end of arming, so
	// they drop out BEFORE the button arms): the hub withholds further frames from them and
	// drops them from the crowd count / round N until they unpark (the Pause control). A
	// no-op for legacy clients, who keep the plain afk ladder. Safe to call for any SteamID
	// (no-op if not connected).
	Park(steamID string)
}

// --- store (the persistent hourly board) ---

// HourlyDelta is a points increment for one player.
type HourlyDelta struct {
	SteamID string
	Points  int
}

// ScoredClick is one click that took a scoring slot in a round. SlotNo is the
// "click N" (0-based arrival order); OffsetMs is its wire-arrival latency
// measured from the arm (the click's At minus the round's armed_at). Button is the
// board slot id this click CLAIMED (in-memory only, for the touch dwell checks — not
// persisted); 0 if unknown.
type ScoredClick struct {
	SteamID  string
	SlotNo   int
	OffsetMs int
	Button   uint16
}

// CursorActivity is a connected player's mouse activity during a window, supplied by the
// ws hub. The arming AFK pass (checkAfk) reads the ARMING-phase snapshot (ArmingCursor
// activityFn, v7+ only); the round-end checks (checkBusted etc.) read the whole-round
// snapshot (allCursorActivityFn, all non-legacy). Legacy clients send no cursors and the
// hub omits them, so Tracked is true for everyone present.
type CursorActivity struct {
	// Tracked is whether this player can be judged at all (a connected non-legacy
	// client). SawCursor is whether any cursor message arrived in the window. Moved is
	// whether the cursor changed position after its anchor (the window's second sample;
	// the first is dropped as a pre-layout transient). AFK == !SawCursor || !Moved.
	Tracked   bool
	SawCursor bool
	Moved     bool

	// Eligible is true when the player was present at the START of the window (the arming
	// stage). A mid-window join, or a connection that hasn't seen a window yet, is not
	// eligible: it never had a fair chance to play, so the AFK nudge skips it.
	Eligible bool
}

// CheckResult is one anticheat check a round flagged against a player. Type is the rule
// that fired:
//   - movement (whole roster):  'afk' (still during arming → parked before the arm).
//   - score-aware (round end):  'busted' (scored with no cursor all round), 'fast_clicks',
//     'too_many_clicks', 'dominant_winner', 'fast_reaction' (#1), 'impossible_latency' (#2),
//     'metronome' (#3), 'no_hover' / 'fast_hover' (touch, #5), 'straight_path' (#6).
//   - session level:            'solo_round'.
// Detail is a short audit note ('delta=84ms' / 'clicks=37' / 'no_cursor' / 'dwell=12ms');
// Message is the player-facing line shown in the anticheat popup. Score NEVER enters the
// 'afk' verdict — the wire-bot "scored without moving" signature is the separate 'busted'.
type CheckResult struct {
	SteamID string
	Type    string
	Detail  string
	Message string
}

// BountyInfo is the snapshot of the active bounty the anticheat code needs each
// game: who leads it (and by how many games-won), when it resolves, and its id
// (which scopes the sanction ladder — counts reset when the id changes). Supplied
// by SetBountyInfoFn; Active is false when no bounty is running.
type BountyInfo struct {
	ID          int64
	LeaderID    string
	LeadMargin  int   // leader's games-won minus the runner-up's; when alone on the board, the leader's own total
	ResolveAtMs int64 // epoch ms the bounty's winner locks in (0 if unknown)
	Active      bool
}

// Sanction is a player's persisted anticheat ladder state within one bounty. The
// engine keeps the live copy in memory and mirrors it to the Store so it survives
// a restart and feeds the admin surface. CooldownUntil is nil until the cooldown
// threshold is crossed; Ignored sidelines them until the bounty resolves.
type Sanction struct {
	SteamID       string
	Checks        int
	CooldownUntil *time.Time
	Ignored       bool
}

// RoundLog is the durable record of one round: its identity, parameters, arm
// time, the scoring clicks in arrival order, and any anticheat checks it flagged.
type RoundLog struct {
	RoundID string
	RoundNo int
	N       int
	Players int
	ArmedAt time.Time
	Clicks  []ScoredClick
	Checks  []CheckResult
	// Replay is the round's visualization payload (buttons, claims, cursor paths),
	// accumulated in memory and flushed with the game history for the admin replay
	// viewer. Not mapped to any game_rounds column — it rides the game's replay blob.
	Replay RoundReplay
}

// TestRecord is one anticheat test the engine issued to a flagged player, handed
// to the Store for the audit trail. ID is the token a correct answer must echo.
type TestRecord struct {
	ID       string
	SteamID  string
	Kind     string
	Prompt   string
	Expected string
}

// TestFrame is an anticheat frame pushed to a single flagged player (the hub
// targets it by SteamID). State is the ladder rung:
//   - "test"     — answer Prompt (echoing ID) to clear the bench.
//   - "cooldown" — sidelined until UntilMs (a timed cooldown); no test.
//   - "ignored"  — sidelined until UntilMs (the bounty resolve time); no test.
// Message is the player-facing explanation for every state. Cleared=true tells
// the client to dismiss any overlay (the player is back in play).
type TestFrame struct {
	State   string
	ID      string
	Kind    string
	Prompt  string
	Message string
	UntilMs int64
	Cleared bool
}

// GameLog is the durable record of one completed game, accumulated in memory
// across the game and flushed once at game end (off the hot path).
type GameLog struct {
	GameID    string
	StartedAt time.Time
	EndedAt   time.Time
	Rounds    int
	RoundLogs []RoundLog
}

// hadPlayers reports whether anyone was connected for any round of the game —
// the gate for persisting it (an empty server's games aren't recorded).
func (l GameLog) hadPlayers() bool {
	for _, r := range l.RoundLogs {
		if r.Players > 0 {
			return true
		}
	}
	return false
}

// Store persists scoring. bucket is the UTC clock-hour the points belong to.
// AddSessionWin credits one game ("session") win to the steamID that topped a
// completed game's final standings. RecordGame writes the full game history
// (games/rounds/scoring clicks) in one batch at game end.
type Store interface {
	AddHourlyPoints(ctx context.Context, bucket time.Time, deltas []HourlyDelta) error
	AddSessionWin(ctx context.Context, steamID string) error
	RecordGame(ctx context.Context, log GameLog) error
	// RecordTestSent records an anticheat test as it is issued; RecordTestAnswer
	// settles it with the player's answer (both wrong and right are recorded).
	RecordTestSent(ctx context.Context, t TestRecord) error
	RecordTestAnswer(ctx context.Context, id, answer string, correct bool) error
	// LoadSanctions returns the anticheat ladder state for every player with a row
	// in the given bounty; SaveSanction upserts one player's state. Together they
	// persist the per-bounty cooldown/ignore ladder across restarts.
	LoadSanctions(ctx context.Context, bountyID int64) (map[string]Sanction, error)
	SaveSanction(ctx context.Context, bountyID int64, s Sanction) error
}

// --- engine ---

type playerInfo struct {
	tag      string
	username string
}

// Engine drives the global game. Construct with New, then call Run.
type Engine struct {
	cfg   Config
	bc    Broadcaster
	store Store
	log   *zap.Logger

	// tickInterval is the live-window broadcast cadence (time.Second / cfg.TickHz),
	// precomputed once; 0 disables ticking. See race.
	tickInterval time.Duration

	clicks chan ClickEvent

	// wake nudges Run to re-check the player count, sent by the hub when a client
	// connects so a paused (empty-server) engine starts a game immediately.
	// Buffered (1) so a connect that races the engine's check isn't lost.
	wake chan struct{}

	rngMu sync.Mutex
	rng   *rand.Rand

	mu      sync.RWMutex // guards phase/round/devNote (read by Snapshot)
	phase   Phase
	round   int
	devNote string // current broadcast note, refreshed once per game

	// badClicks counts non-scoring ("idle") clicks per SteamID accumulated since the
	// last arm, across EVERY phase — arming, the live window, result display, game
	// over and intermission. Touched only from the Run goroutine (pending/race/drain),
	// so it needs no lock. Logged and reset at each arm as TELEMETRY only (the delayed-arm
	// penalty it once drove was gutted in v7); see recordBad.
	badClicks map[string]int

	// underTest is the set of SteamIDs benched by a failed anticheat check: they
	// are withheld from the armed frame until they pass a test. Only test-capable
	// clients are added (older clients still have checks run/logged, never benched).
	// pendingTests holds each benched player's outstanding test (the answer must
	// echo its id). Both are touched only from the Run goroutine.
	underTest    map[string]bool
	pendingTests map[string]pendingTest

	// answers carries client test answers from the hub into the Run goroutine,
	// like clicks. Buffered; a full buffer drops (the player can re-submit).
	answers chan answerEvent

	// extSanction carries admin-set sanction overrides (e.g. an edited flag count)
	// into the Run goroutine so they apply live within the current bounty rather than
	// only on the next bounty reload. Buffered; a full buffer drops (admin re-submits).
	extSanction chan extSanctionEvent

	// gameEndHook, if set, runs after each game_over (post session-win write);
	// used to refresh the leaderboard cache once per "session". Optional.
	gameEndHook func(context.Context)

	// devNoteFn, if set, supplies the host-editable broadcast note; it is called
	// once at the start of each game (so config edits apply on the next game).
	// Optional — nil means no note is ever sent. Call SetDevNoteFn before Run.
	devNoteFn func() string

	// bountyInfoFn, if set, returns the active bounty snapshot (id, leader + margin,
	// resolve time). Used by the session-level solo_round check (leader + margin
	// gating) and by the sanction ladder (the bounty id scopes counts; the resolve
	// time bounds the "ignored" rung). Optional; nil disables solo_round and the
	// ladder runs with a zero bounty id (counts never reset, no resolve-time
	// countdown). Read from the in-memory leaderboard cache, so it's cheap to call.
	bountyInfoFn func() BountyInfo

	// allCursorActivityFn, if set, returns the WHOLE-ROUND cursor activity (arming +
	// armed) of every connected non-legacy player, read at round END by the score-aware
	// checks (busted: scored with no cursor all round). Supplied by the ws hub. Optional.
	allCursorActivityFn func() map[string]CursorActivity

	// armingCursorActivityFn, if set, returns the ARMING-phase cursor activity of every
	// connected v7+ player (v6 sends cursors armed-only and is exempt). Read after
	// pending() returns, before the arm, by the movement-only afk pass (checkAfk), which
	// parks a still player BEFORE the button arms. Optional: nil disables the afk pass.
	armingCursorActivityFn func() map[string]CursorActivity

	// touchDataFn, if set, returns each connected v7+ player's first-touch offsets (button
	// id → ms since the window origin) for the round just played, for the no_hover /
	// fast_hover dwell checks. Supplied by the ws hub. Optional: nil disables them.
	touchDataFn func() map[string]map[uint16]int

	// minRTTFn, if set, returns each connected player's minimum observed ping RTT (ms),
	// for impossible_latency. Supplied by the ws hub. Optional: nil disables that check.
	minRTTFn func() map[string]int

	// cursorTracksFn, if set, returns the full recorded cursor path of every connected
	// non-legacy player during the window just played, for the durable game replay.
	// Supplied by the ws hub (the same per-window capture the afk pass reads, but the
	// whole sample list rather than a movement summary). Optional: nil means replays
	// carry no cursor dots.
	cursorTracksFn func() map[string]CursorTrack

	// Anticheat sanction ladder, scoped to the current bounty. curBountyID is the
	// bounty the in-memory sanctions belong to; when bountyInfoFn reports a different
	// id the ladder resets (reloaded from the store for the new bounty). sanctions
	// is the live per-player state. Touched only from the Run goroutine.
	curBountyID     int64
	bountyLoaded    bool
	bountyResolveMs int64                // active bounty's resolve time (the "ignored" countdown target)
	sanctions       map[string]*Sanction // per-player ladder state for the current bounty
	benchMsg        map[string]string    // last triggering check message per test-rung player
	notified        map[string]string    // last sanction state pushed to each client ("test"/"cooldown"/"ignored")

	// sanctionSnap is a lock-guarded copy of the sanction state, republished from the
	// Run goroutine after every change, so other goroutines (the API serving the
	// leaderboard status dots) can read each player's rung without racing the Run
	// loop's maps. Guarded by sanctionMu.
	sanctionMu   sync.RWMutex
	sanctionSnap map[string]Sanction
}

// SetGameEndHook registers a callback run after every game_over, once the
// session win has been persisted. Call before Run; nil clears it.
func (e *Engine) SetGameEndHook(fn func(context.Context)) { e.gameEndHook = fn }

// SetDevNoteFn registers the source of the per-game dev note. Call before Run;
// nil clears it.
func (e *Engine) SetDevNoteFn(fn func() string) { e.devNoteFn = fn }

// SetBountyInfoFn registers the source of the active bounty snapshot (used by the
// solo_round check and the sanction ladder). Call before Run; nil disables both.
func (e *Engine) SetBountyInfoFn(fn func() BountyInfo) { e.bountyInfoFn = fn }

// SetAllCursorActivityFn registers the source of whole-round (arming + armed) cursor
// activity, read at round end by the score-aware checks (busted). Call before Run.
func (e *Engine) SetAllCursorActivityFn(fn func() map[string]CursorActivity) {
	e.allCursorActivityFn = fn
}

// SetArmingCursorActivityFn registers the source of arming-phase cursor activity (v7+),
// read by the movement-only afk pass between pending() and the arm. Call before Run; nil
// disables the afk pass. See checkAfk.
func (e *Engine) SetArmingCursorActivityFn(fn func() map[string]CursorActivity) {
	e.armingCursorActivityFn = fn
}

// SetTouchDataFn registers the source of per-window touch data (used by no_hover /
// fast_hover). Call before Run; nil disables the touch checks.
func (e *Engine) SetTouchDataFn(fn func() map[string]map[uint16]int) { e.touchDataFn = fn }

// SetMinRTTFn registers the source of per-connection min ping RTT (used by
// impossible_latency). Call before Run; nil disables that check.
func (e *Engine) SetMinRTTFn(fn func() map[string]int) { e.minRTTFn = fn }

// Phase returns the engine's current round phase. Safe from any goroutine (lock-read);
// the hub reads it to decide whether a park/unpark request is deferred (Armed) or applied
// immediately (everything else).
func (e *Engine) Phase() Phase {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.phase
}

// SetCursorTracksFn registers the source of whole-roster cursor paths (used to build
// the durable game replay). Call before Run; nil means replays carry no cursor dots.
func (e *Engine) SetCursorTracksFn(fn func() map[string]CursorTrack) {
	e.cursorTracksFn = fn
}

// cursorTracks returns the per-window cursor paths from the hub (nil if unset).
func (e *Engine) cursorTracks() map[string]CursorTrack {
	if e.cursorTracksFn == nil {
		return nil
	}
	return e.cursorTracksFn()
}

// New builds an Engine. store may be nil (scoring still works, just not
// persisted). log may be nil (falls back to a no-op logger).
func New(cfg Config, bc Broadcaster, store Store, log *zap.Logger) *Engine {
	if log == nil {
		log = zap.NewNop()
	}
	tickInterval := time.Duration(0)
	if cfg.TickHz > 0 {
		tickInterval = time.Second / time.Duration(cfg.TickHz)
	}
	return &Engine{
		cfg:    cfg,
		bc:     bc,
		store:  store,
		log:    log,
		tickInterval: tickInterval,
		clicks:       make(chan ClickEvent, 4096),
		wake:         make(chan struct{}, 1),
		answers:      make(chan answerEvent, 256),
		extSanction:  make(chan extSanctionEvent, 64),
		rng:          rand.New(rand.NewSource(seedFromCrypto())),
		phase:        PhaseIntermission,
		badClicks:    map[string]int{},
		underTest:    map[string]bool{},
		pendingTests: map[string]pendingTest{},
		sanctions:    map[string]*Sanction{},
		benchMsg:     map[string]string{},
		notified:     map[string]string{},
		sanctionSnap: map[string]Sanction{},
	}
}

// answerEvent is one client test answer as the hub read it: the test token it
// answers and the submitted text.
type answerEvent struct {
	SteamID string
	ID      string
	Answer  string
}

// SubmitAnswer hands a client's test answer to the engine. Non-blocking (drops
// if the buffer is full; the player can re-submit). Safe from any goroutine.
func (e *Engine) SubmitAnswer(ev answerEvent) {
	select {
	case e.answers <- ev:
	default:
	}
}

// NewAnswer builds an answerEvent for the hub (which has no view of the engine's
// unexported types).
func NewAnswer(steamID, id, answer string) answerEvent {
	return answerEvent{SteamID: steamID, ID: id, Answer: answer}
}

// extSanctionEvent is an admin-set sanction override for one player, applied in
// the Run goroutine. The store row has already been written; this updates the
// engine's live in-memory copy so the change takes effect this bounty.
type extSanctionEvent struct {
	SteamID string
	S       Sanction
}

// SetSanction applies an admin-set sanction state for a player live (e.g. an
// edited flag count, with cooldown/ignored cleared). Non-blocking; safe from any
// goroutine. The caller persists the same state to the store.
func (e *Engine) SetSanction(steamID string, s Sanction) {
	s.SteamID = steamID
	select {
	case e.extSanction <- extSanctionEvent{SteamID: steamID, S: s}:
	default:
	}
}

// Submit hands a click to the engine. Non-blocking: if the buffer is full (a
// pathological in-rush) the click is dropped rather than blocking the hub's read
// goroutine. Safe to call from any goroutine.
func (e *Engine) Submit(ev ClickEvent) {
	select {
	case e.clicks <- ev:
	default:
	}
}

// Snapshot is the current phase/round (plus the live player count and the N a
// round would take right now), for the hello frame. Safe from any goroutine.
type Snapshot struct {
	Phase     Phase
	Round     int
	Of        int
	Players   int
	Clicks    int
	ArmMinSec int // arming-window bounds (the per-round delay itself stays secret)
	ArmMaxSec int
	// DevNote is the current host-editable broadcast note (empty = none); carried
	// in hello so a mid-game joiner sees it without waiting for the next game.
	DevNote string
	// TickMs is the live-window tick interval in ms (0 = ticking disabled), sent on
	// connect so the client can size its pip jitter-buffer playback delay (D ≥ one
	// tick interval + jitter margin) to the server's cadence.
	TickMs int
}

func (e *Engine) Snapshot() Snapshot {
	e.mu.RLock()
	phase, round, devNote := e.phase, e.round, e.devNote
	e.mu.RUnlock()
	players := 0
	if e.bc != nil {
		players = e.bc.PlayerCount()
	}
	return Snapshot{
		Phase: phase, Round: round, Of: e.cfg.RoundsPerGame,
		Players: players, Clicks: e.clicksFor(players),
		ArmMinSec: int(e.cfg.ArmMin / time.Second), ArmMaxSec: int(e.cfg.ArmMax / time.Second),
		DevNote: devNote,
		TickMs:  int(e.tickInterval / time.Millisecond),
	}
}

// CursorSampleK is the max opponent cursors the hub samples into each tick frame
// (Config.TickSampleK, repurposed now that claims are complete rather than sampled).
func (e *Engine) CursorSampleK() int { return e.cfg.TickSampleK }

// clicksFor is the per-round N: the scoring slots scale with the connected
// crowd (ClicksPerPlayer each), floored so a near-empty server still races.
func (e *Engine) clicksFor(players int) int {
	n := e.cfg.ClicksPerPlayer * players
	if n < e.cfg.MinClicks {
		n = e.cfg.MinClicks
	}
	if n < 1 {
		n = 1
	}
	return n
}

func (e *Engine) setPhase(p Phase, round int) {
	e.mu.Lock()
	e.phase = p
	e.round = round
	e.mu.Unlock()
}

func (e *Engine) setDevNote(note string) {
	e.mu.Lock()
	e.devNote = note
	e.mu.Unlock()
}

// Run drives games until ctx is cancelled. Blocks; call in its own goroutine.
// It pauses while no one is connected (see waitForPlayers) so an idle server
// neither runs games nor writes empty history; a connecting client starts a
// fresh game at once.
func (e *Engine) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if !e.waitForPlayers(ctx) {
			return
		}
		e.playGame(ctx)
		if ctx.Err() != nil {
			return
		}
		e.setPhase(PhaseIntermission, 0)
		e.drain(ctx, e.cfg.Intermission)
	}
}

// Wake nudges the engine to re-check the player count. The hub calls it when a
// client connects, so a paused (empty-server) engine starts a game immediately.
// Non-blocking and safe from any goroutine.
func (e *Engine) Wake() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// waitForPlayers blocks until at least one (non-legacy) client is connected, so
// the engine doesn't run games — and write empty game history — on an empty
// server. A connecting client wakes it via Wake(); it returns false only if ctx
// is cancelled while waiting. With no Broadcaster wired (unit tests) it never
// pauses.
func (e *Engine) waitForPlayers(ctx context.Context) bool {
	if e.bc == nil {
		return true
	}
	for e.bc.PlayerCount() == 0 {
		e.setPhase(PhaseIntermission, 0)
		select {
		case <-ctx.Done():
			return false
		case <-e.wake:
			// A client connected (or churned) — loop and re-check the count.
		case <-e.clicks:
			// Stray click with nobody counted; discard so the channel can't back up.
		case <-e.answers:
			// Stray test answer with nobody counted; discard so it can't back up.
		case ev := <-e.extSanction:
			// Admin sanction edit while idle — apply it so it's in place once play resumes.
			e.applyExternalSanction(ev)
		}
	}
	return true
}

func (e *Engine) playGame(ctx context.Context) {
	gameID := newID()
	startedAt := time.Now().UTC()
	scores := map[string]int{}           // cumulative game points by SteamID
	info := map[string]playerInfo{}      // latest display info by SteamID
	reached := map[string]time.Time{}    // when each player reached their score (tie-break)
	roundLogs := []RoundLog{}            // durable per-round history, flushed at game end
	x := e.cfg.RoundsPerGame

	// Refresh the host-editable broadcast note once per game and push it to every
	// client (empty clears it on the client — "shows until an empty note is sent").
	note := ""
	if e.devNoteFn != nil {
		note = e.devNoteFn()
	}
	e.setDevNote(note)
	e.bc.DevNote(note)

	// Sync the anticheat sanction ladder to the active bounty once per game: if the
	// bounty changed since last game the per-bounty counts reset (reloaded from the
	// store for the new bounty), and anyone the old bounty had sidelined is cleared.
	bi := e.bountyInfo()
	e.syncBounty(ctx, bi)

	// The final round's points + round id, carried into game_over (the last round
	// has no separate round_result — see race/final).
	var finalDeltas map[string]int
	var finalRoundID string
	// Every player who scored a point in ANY round this session. The session-level
	// solo_round check (after the loop) keys off this: uncontested == the leader is
	// the only entry here.
	sessionScorers := map[string]bool{}
	// AFK fires at most ONCE per game per player. This breaks the answer-a-test-mid-round
	// re-flag loop within a game, while a player who is AFK across multiple games still
	// ladders out (the set resets each game). Reset here.
	afkFired := map[string]bool{}
	for round := 1; round <= x && ctx.Err() == nil; round++ {
		// players is the full connected crowd (shown to clients + recorded); N is sized
		// to the players who can actually race — benched/cooled-down/ignored players are
		// excluded so they don't inflate the scoring slots.
		players := e.bc.PlayerCount()
		n := e.clicksFor(e.bc.ActivePlayerCount(e.blockedMap()))
		armingStart := time.Now() // the window origin (the replay's arming-origin timeline)
		e.pending(ctx, round, x, players, n, info)
		if ctx.Err() != nil {
			return
		}

		// ARMING-PHASE AFK pass (issue #43): AFK is now a purely movement signal evaluated
		// at the END of arming, before the button arms — a still player is parked so they
		// never take a scoring slot. Score NEVER enters this (the wire-bot "scored without
		// moving" signature is the round-end `busted` check). blocked is re-snapshotted
		// after pending()'s notifySanctions (a player may have cleared their test there).
		blocked := e.blockedMap()
		afkChecks := e.checkAfk(round, blocked, afkFired)
		for _, ch := range afkChecks {
			if blocked[ch.SteamID] {
				continue
			}
			if e.bc != nil && e.bc.TestCapable(ch.SteamID) {
				e.applySanction(ch, bi)
				e.bc.Park(ch.SteamID) // afk always parks now — drop them off the board before the arm
			}
		}
		// Parking changed the active roster — re-snapshot the crowd and re-size N for the
		// armed frame so parked players don't inflate the scoring slots.
		players = e.bc.PlayerCount()
		n = e.clicksFor(e.bc.ActivePlayerCount(e.blockedMap()))

		final := round == x
		deltas, roundID, clicks, armedAt, replay := e.race(ctx, round, x, players, n, armingStart, scores, info, reached, final)
		if ctx.Err() != nil {
			return
		}
		scoredThisRound := map[string]bool{}
		for _, c := range clicks {
			scoredThisRound[c.SteamID] = true
			sessionScorers[c.SteamID] = true
		}
		// Round-end, SCORE-AWARE checks (the only ones that read scoring). blocked =
		// players already benched / cooled / ignored this bounty; they are skipped from
		// flagging (a player working through a test must not pile up fresh flags). The afk
		// verdicts (raised above) are folded into the round's audit log alongside these.
		blocked = e.blockedMap()
		checks := e.runChecks(clicks, checkCtx{n: n})
		checks = append(checks, e.checkBusted(scoredThisRound, blocked)...)
		checks = append(checks, e.checkLatency(clicks, blocked)...)
		checks = append(checks, e.checkTouch(clicks, blocked)...)
		checks = append(checks, e.checkStraightPath(scoredThisRound, blocked)...)
		e.applyRoundChecks(checks, blocked, bi)
		allChecks := append(afkChecks, checks...)
		roundLogs = append(roundLogs, RoundLog{
			RoundID: roundID, RoundNo: round, N: n, Players: players,
			ArmedAt: armedAt, Clicks: clicks, Checks: allChecks, Replay: replay,
		})
		if final {
			finalDeltas, finalRoundID = deltas, roundID
		}
	}
	if ctx.Err() != nil {
		return
	}

	// Session-level solo_round: with the whole session played out, flag the bounty
	// leader if it was uncontested (they were the only scorer in any round) and their
	// board lead clears SoloLeadMargin. Attached to the final round's log so it FKs
	// cleanly to a persisted game_rounds row, and sanctioned like a per-round flag.
	// Skipped, like the per-round checks, while the leader already has an outstanding
	// test / cooldown so it never piles onto an unanswered rung.
	if ch := e.checkSoloSession(sessionScorers, bi.LeaderID, bi.LeadMargin); ch != nil {
		if e.bc != nil && !e.blockedMap()[ch.SteamID] && e.bc.TestCapable(ch.SteamID) {
			e.applySanction(*ch, bi)
		}
		if len(roundLogs) > 0 {
			last := &roundLogs[len(roundLogs)-1]
			last.Checks = append(last.Checks, *ch)
		}
	}

	final := standingsOf(scores, info, reached)
	stampBehindMs(final, reached) // tie-break margins on the FINAL game standings only
	e.annotateStatus(final)
	placements := make(map[string]int, len(final))
	won := make(map[string]bool, len(final))
	for i, s := range final {
		placements[s.SteamID] = i + 1
		won[s.SteamID] = i == 0 // placement 1 == win
	}
	e.bc.GameOver(GameOverFrame{
		Standings:  topK(final, e.cfg.BoardSize),
		GameID:     gameID,
		Placements: placements,
		Won:        won,
		Deltas:     finalDeltas,
		RoundID:    finalRoundID,
	})

	// Post-game work runs off the hot path (we're entering intermission) in one
	// goroutine so it's ordered: write the game history, credit the session win,
	// then fire the game-end hook (the leaderboard-cache refresh).
	gameLog := GameLog{
		GameID: gameID, StartedAt: startedAt, EndedAt: time.Now().UTC(),
		Rounds: x, RoundLogs: roundLogs,
	}
	go e.afterGame(final, gameLog)
}

// afterGame persists the completed game's history, credits the session win to
// the game's top scorer (if anyone scored), and then runs the optional game-end
// hook. Detached context so a shutdown right after game_over still records and
// refreshes.
func (e *Engine) afterGame(final []Standing, log GameLog) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Don't persist a game nobody was present for. Run pauses on an empty server,
	// but a client can still connect and drop between the player-count check and
	// the first arm, yielding an all-empty game — skip those.
	if e.store != nil && log.hadPlayers() {
		if err := e.store.RecordGame(ctx, log); err != nil {
			e.log.Error("persist game history", zap.Error(err))
		}
	}

	if len(final) > 0 && e.store != nil {
		if err := e.store.AddSessionWin(ctx, final[0].SteamID); err != nil {
			e.log.Error("persist session win", zap.Error(err))
		}
	}
	if e.gameEndHook != nil {
		e.gameEndHook(ctx)
	}
}

// pending is the IDLE/arming phase: announce the round, then wait the secret delay.
// Cursors sent during it are now captured (the window origin), so the arming-phase AFK
// pass (in playGame, after this returns) has a movement signal before the arm. Idle clicks
// during it score nothing but accrue this connection's bad-click telemetry (e.badClicks).
func (e *Engine) pending(ctx context.Context, round, of, players, n int, info map[string]playerInfo) {
	e.setPhase(PhasePending, round)
	e.bc.Pending(PendingFrame{Round: round, Of: of, Players: players, Clicks: n})

	// Re-notify sanctioned players during the arming window: resend math tests so a
	// benched player can clear the gate before this round arms, refresh cooldown /
	// ignored countdowns, and clear anyone whose cooldown elapsed.
	e.notifySanctions()

	timer := time.NewTimer(e.randArmDelay())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-e.clicks:
			recordInfo(info, ev)
			e.recordBad(ev)
		case ans := <-e.answers:
			e.handleAnswer(ans)
		case ev := <-e.extSanction:
			e.applyExternalSanction(ev)
		case <-timer.C:
			return
		}
	}
}

// race is the ARMED phase: arm with a fresh nonce, accept the first N valid
// clicks (by arrival), then score them. Closes the instant click N lands. It
// returns the round's per-SteamID points deltas, its round id, the scoring
// clicks in arrival order (for the durable history), and the arm time.
//
// On a non-final round it then publishes round_result and pauses for the result
// display. On the FINAL round (final==true) it scores and returns but publishes
// nothing — playGame folds straight into game_over (which carries the returned
// deltas/roundID), so the last round shows the final standings once instead of a
// redundant ROUND OVER → GAME OVER pair.
func (e *Engine) race(ctx context.Context, round, of, players, n int, armingStart time.Time, scores map[string]int, info map[string]playerInfo, reached map[string]time.Time, final bool) (map[string]int, string, []ScoredClick, time.Time, RoundReplay) {
	blocked := e.blockedMap()
	e.setPhase(PhaseArmed, round)
	armedAt := time.Now()

	// Build the live board: mint up to ButtonsOnScreen buttons, each a fresh nonce at a
	// non-overlapping server-RNG'd position. The initial set rides the armed frame; mint
	// also refills the board after each claim (see board.offer).
	b := newBoard(n, armedAt)
	b.mint = func() Button {
		x, y := e.randPos(b.positions())
		return b.register(newNonce(), x, y)
	}
	buttons := make([]Button, 0, e.cfg.ButtonsOnScreen)
	for i := 0; i < e.cfg.ButtonsOnScreen; i++ {
		buttons = append(buttons, b.mint())
	}

	e.bc.Armed(ArmedFrame{Round: round, Seq: round, Buttons: buttons, Players: players, Clicks: n, Blocked: blocked})
	// The delayed-arm spam penalty was gutted in v7 (see the type doc) — the arm is now a
	// single broadcast. The bad-click tally is kept as TELEMETRY only: log this arm's
	// dormant-click counts before resetting, so the signal is still visible (and available
	// for a future high-threshold flag) without holding back anyone's window.
	if len(e.badClicks) > 0 {
		total := 0
		for _, c := range e.badClicks {
			total += c
		}
		e.log.Info("idle_clicks", zap.Int("round", round), zap.Int("connections", len(e.badClicks)), zap.Int("total", total))
	}
	e.badClicks = map[string]int{}

	timer := time.NewTimer(e.cfg.RaceMax)
	defer timer.Stop()

	// Live-window tick: every tickInterval, broadcast the running clicks-remaining count
	// plus the board mutations (claims + their replacements) accumulated since the last
	// tick. takePending drains them so each tick carries only the new ones; the hub adds
	// the sampled opponent cursors. A nil tickC (ticking disabled) simply never fires.
	var tickC <-chan time.Time
	if e.tickInterval > 0 {
		t := time.NewTicker(e.tickInterval)
		defer t.Stop()
		tickC = t.C
	}
	emitTick := func() {
		e.bc.Tick(TickFrame{Round: round, Remaining: n - len(b.scored), Claims: b.takePending()})
	}

raceLoop:
	for !b.full() {
		select {
		case <-ctx.Done():
			return nil, "", nil, armedAt, RoundReplay{}
		case ev := <-e.clicks:
			recordInfo(info, ev)
			if !b.offer(ev) {
				e.recordBad(ev) // an idle click during the live window still penalises
			}
		case ans := <-e.answers:
			e.handleAnswer(ans)
		case <-tickC:
			emitTick()
		case <-timer.C:
			break raceLoop // safety: fewer than N clicks arrived
		}
	}
	// Flush the final batch: the claims that closed the race (including the winning
	// click and its nil-Spawn) landed after the last tick, so emit one more so the
	// count hits its floor and the decisive claims go out — they drain over
	// round_result client-side.
	if e.tickInterval > 0 {
		emitTick()
	}

	// Assemble the round's replay while the hub still holds this window's cursor capture
	// (the next round's pending() clears it). The board's never-drained claim log + the
	// initial buttons give the full button timeline; the hub supplies the cursor paths.
	// armMs = the arming duration (window origin → arm); cursors are arming-origin, so the
	// button/claim offsets are shifted onto the same timeline (see buildRoundReplay).
	armMs := int(armedAt.Sub(armingStart).Milliseconds())
	if armMs < 0 {
		armMs = 0
	}
	replay := e.buildRoundReplay(round, n, armMs, int(time.Since(armedAt).Milliseconds()), buttons, b)

	deltas := map[string]int{}
	clicks := make([]ScoredClick, len(b.scored))
	for i, ev := range b.scored {
		deltas[ev.SteamID]++
		scores[ev.SteamID]++
		// Record the arrival time of each scoring click; the last one wins, so
		// reached[sid] ends up as when this player reached their current total —
		// the tie-break standingsOf uses ("who got there first"). b.log[i] is the claim
		// this click made (same arrival order as b.scored), giving the button it claimed
		// for the touch dwell checks.
		reached[ev.SteamID] = ev.At
		clicks[i] = ScoredClick{SteamID: ev.SteamID, SlotNo: i, OffsetMs: int(ev.At.Sub(armedAt).Milliseconds()), Button: b.log[i].SlotID}
	}
	if len(deltas) > 0 && e.store != nil {
		e.persist(deltas)
	}
	roundID := newID()

	// Final round: no separate round_result or result-display pause — playGame
	// emits game_over next, carrying these deltas/roundID.
	if final {
		return deltas, roundID, clicks, armedAt, replay
	}

	e.setPhase(PhaseResult, round)

	// One entry per scoring player in first-arrival order; a masher who took
	// several slots still shows once. Points is the score from THIS round (the
	// delta), not the cumulative game total — the standings list already carries
	// the running totals, so the winners list is the per-round "round scores" the
	// client flashes after each round.
	winners := make([]Standing, 0, len(deltas))
	seen := make(map[string]bool, len(deltas))
	for _, ev := range b.scored {
		if seen[ev.SteamID] {
			continue
		}
		seen[ev.SteamID] = true
		pi := info[ev.SteamID]
		winners = append(winners, Standing{Tag: pi.tag, Username: pi.username, Points: deltas[ev.SteamID], SteamID: ev.SteamID})
	}
	e.annotateStatus(winners)
	standings := topK(standingsOf(scores, info, reached), e.cfg.BoardSize)
	e.annotateStatus(standings)
	e.bc.Result(ResultFrame{
		Round:     round,
		Of:        of,
		Winners:   winners,
		Standings: standings,
		RoundID:   roundID,
		Deltas:    deltas,
	})

	e.drain(ctx, e.cfg.ResultDisplay)
	return deltas, roundID, clicks, armedAt, replay
}

// persist writes the round's points to the hourly board off the hot path, so DB
// latency never delays the next round. A detached context lets a shutdown
// mid-round still record points.
func (e *Engine) persist(deltas map[string]int) {
	bucket := time.Now().UTC().Truncate(time.Hour)
	ds := make([]HourlyDelta, 0, len(deltas))
	for sid, pts := range deltas {
		ds = append(ds, HourlyDelta{SteamID: sid, Points: pts})
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := e.store.AddHourlyPoints(ctx, bucket, ds); err != nil {
			e.log.Error("persist hourly points", zap.Error(err))
		}
	}()
}

// drain sleeps for d while discarding any clicks, so the channel never backs up
// during result/intermission (those clicks score nothing). Returns early on ctx.
func (e *Engine) drain(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-e.clicks:
			e.recordBad(ev) // result/intermission/game-over clicks still accrue toward the next arm
		case ans := <-e.answers:
			e.handleAnswer(ans)
		case ev := <-e.extSanction:
			e.applyExternalSanction(ev)
		case <-timer.C:
			return
		}
	}
}

func (e *Engine) randArmDelay() time.Duration {
	min, max := e.cfg.ArmMin, e.cfg.ArmMax
	if max <= min {
		return min
	}
	e.rngMu.Lock()
	n := e.rng.Int63n(int64(max-min) + 1)
	e.rngMu.Unlock()
	return min + time.Duration(n)
}

// --- pure helpers (unit-tested directly) ---

// recordBad bumps a connection's cross-phase bad-click ("idle", zero-nonce) tally. The
// delayed-arm penalty this once fed was gutted in v7; the tally is kept as TELEMETRY only
// (logged per arm in race, see CLAUDE.md / issue #43), never an auto-sanction — dormant
// mashing is human (eager players mash before the arm) and the nonce + rate limiter + the
// anticheat checks already cover blind flooding. Non-zero-nonce clicks are race attempts,
// never counted even when they lose.
func (e *Engine) recordBad(ev ClickEvent) {
	if ev.Nonce == 0 {
		e.badClicks[ev.SteamID]++
	}
}

// --- anticheat: checks + tests ---

// checkCtx is the round context the per-round checks need beyond the scoring
// clicks: n, the round's scoring-slot count. The too_many_clicks divisor is
// derived from the scoring clicks themselves (how many players actually scored
// this round), not from a connection count. (solo_round is NOT a per-round check;
// it's evaluated once across the whole session by checkSoloSession.)
type checkCtx struct {
	n int
}

// runChecks inspects a round's scoring clicks and returns one CheckResult per
// (player, rule) that fired, each carrying a player-facing Message. The rules,
// all using only this round's scoring clicks (already in wire-arrival order):
//   - fast_clicks:     two consecutive scoring clicks < FastClickMs apart.
//   - too_many_clicks: more than MaxClickFactor × the round's fair share
//                      (N / players who scored this round); needs ≥2 scorers.
//   - dominant_winner: top scorer took > 2× a runner-up who actually competed
//                      (scored ≥ DominantRunnerUpMin).
//
// The cursor-based afk checks are deliberately absent here: they are NOT scoring-click
// rules, so they run in their own whole-roster pass (checkAfk), called alongside this
// from playGame. solo_round is likewise absent (a session-level verdict in
// checkSoloSession).
func (e *Engine) runChecks(clicks []ScoredClick, c checkCtx) []CheckResult {
	if len(clicks) == 0 {
		return nil
	}
	// Per-player scoring-click offsets, preserving arrival order.
	offsets := map[string][]int{}
	order := []string{} // stable iteration so results are deterministic
	for _, ev := range clicks {
		if _, seen := offsets[ev.SteamID]; !seen {
			order = append(order, ev.SteamID)
		}
		offsets[ev.SteamID] = append(offsets[ev.SteamID], ev.OffsetMs)
	}

	// too_many_clicks limit: MaxClickFactor × this round's fair share of slots,
	// where the share is N / the number of players who ACTUALLY scored this round
	// (len(order)). The connected/active crowd is deliberately NOT the divisor —
	// idle-but-connected lurkers would shrink the "fair share" and flag a lone
	// clicker who legitimately took the whole window with nobody racing them. Needs
	// ≥2 distinct scorers; with one there is no share to exceed (same gate as
	// dominant_winner). N may exceed ClicksPerPlayer when the MinClicks floor lifts it.
	limit := 0
	if e.cfg.MaxClickFactor > 0 && len(order) >= 2 {
		fair := c.n / len(order)
		if fair < 1 {
			fair = 1
		}
		// Fractional factor (e.g. 2.5) is floored to a whole click count.
		limit = int(e.cfg.MaxClickFactor * float64(fair))
	}

	var out []CheckResult
	for _, sid := range order {
		offs := offsets[sid]
		// fast_clicks: smallest gap between consecutive scoring clicks.
		if e.cfg.FastClickMs > 0 {
			for i := 1; i < len(offs); i++ {
				if d := offs[i] - offs[i-1]; d < e.cfg.FastClickMs {
					out = append(out, CheckResult{SteamID: sid, Type: "fast_clicks",
						Detail:  fmt.Sprintf("delta=%dms", d),
						Message: "You clicked faster than humanly possible."})
					break
				}
			}
		}
		// fast_reaction (#1): the player's FIRST scoring click landed sooner after the arm
		// than any human could react. offs is in arrival order, so offs[0] is their earliest
		// arm→click latency. Catches a one-shot bot that fast_clicks (needs ≥2 clicks) can't.
		if e.cfg.ReactionMinMs > 0 && len(offs) > 0 && offs[0] < e.cfg.ReactionMinMs {
			out = append(out, CheckResult{SteamID: sid, Type: "fast_reaction",
				Detail:  fmt.Sprintf("offset=%dms", offs[0]),
				Message: "You reacted to the button faster than humanly possible."})
		}
		// metronome (#3): a machine-flat cadence (very low jitter) across ≥ MinClicks scoring
		// clicks is an autoclicker signature a human mash never produces. Off by default
		// (MetronomeMinClicks 0). Coefficient of variation = stddev/mean of the gaps.
		if e.cfg.MetronomeMinClicks > 0 && len(offs) >= e.cfg.MetronomeMinClicks {
			if cv, ok := coeffVar(offs); ok && cv < e.cfg.MetronomeMaxCV {
				out = append(out, CheckResult{SteamID: sid, Type: "metronome",
					Detail:  fmt.Sprintf("cv=%.3f n=%d", cv, len(offs)),
					Message: "Your clicks came at a machine-perfect cadence."})
			}
		}
		// too_many_clicks: an implausible share of this round's slots.
		if limit > 0 && len(offs) > limit {
			out = append(out, CheckResult{SteamID: sid, Type: "too_many_clicks",
				Detail:  fmt.Sprintf("clicks=%d limit=%d", len(offs), limit),
				Message: "You took far more of the round's clicks than your share."})
		}
	}

	// dominant_winner: the top scorer took MORE than 2× the runner-up's clicks, but
	// only when the runner-up actually competed (scored ≥ DominantRunnerUpMin) — so a
	// lone clicker beating an idle player is never flagged.
	if len(order) >= 2 {
		topSID, topN, secondN := "", 0, 0
		for _, sid := range order { // stable: first-arrival order breaks count ties
			if n := len(offsets[sid]); n > topN {
				topN, secondN, topSID = n, topN, sid
			} else if n > secondN {
				secondN = n
			}
		}
		if secondN >= e.cfg.DominantRunnerUpMin && topN > 2*secondN {
			out = append(out, CheckResult{SteamID: topSID, Type: "dominant_winner",
				Detail:  fmt.Sprintf("clicks=%d vs %d", topN, secondN),
				Message: "You out-clicked the field by an impossible margin."})
		}
	}
	return out
}

// afkByCursor is the AFK predicate: a player is AFK if no cursor messages arrived at all,
// or the cursor never moved (no sample after the anchor differed from it). It is a pure
// MOVEMENT signal — score never enters it. checkAfk decides what an AFK verdict means.
func afkByCursor(act CursorActivity) bool {
	return !act.SawCursor || !act.Moved
}

// checkAfk is the ARMING-PHASE AFK pass (issue #43). It is run after pending() returns and
// BEFORE the button arms, against the ARMING-window cursor movement of the whole judgeable
// roster (armingCursorActivityFn — v7+ only; v6 sends cursors armed-only and is exempt). A
// player who didn't move their cursor during the arming wait is AFK and is parked by the
// caller BEFORE the arm, so they never take a scoring slot. AFK is purely a movement signal:
// score NEVER enters it (the wire-bot "scored without moving" signature is the round-end
// `busted` check). AfkCheck>0 gates the whole pass.
//
// Only an ELIGIBLE player is flagged (CursorActivity.Eligible: present at the start of this
// arming window). A mid-window join, or a connection that hasn't had a window yet, had no
// fair chance to put its cursor down, so it is skipped. blocked players (already benched /
// cooled / ignored) are logged but never re-flagged. afkFired caps AFK at once per game per
// player (caller-owned, reset each game): a genuinely-AFK player still ladders out across
// games. Results are returned in SteamID order so they are deterministic.
//
// Every judgeable player is LOGGED every round (afk_eval) plus an afk_round summary, so
// stillness and the skips are visible in the logs rather than inferred from a missing line.
func (e *Engine) checkAfk(round int, blocked, afkFired map[string]bool) []CheckResult {
	if e.cfg.AfkCheck <= 0 || e.armingCursorActivityFn == nil {
		return nil
	}
	acts := e.armingCursorActivityFn()
	ids := make([]string, 0, len(acts))
	for sid := range acts {
		ids = append(ids, sid)
	}
	sort.Strings(ids)

	var out []CheckResult
	var afkN, flagged, blockedAfk, ineligible, alreadyFired int
	for _, sid := range ids {
		act := acts[sid]
		afk := act.Tracked && afkByCursor(act)
		e.log.Info("afk_eval",
			zap.Int("round", round),
			zap.String("sid", sid),
			zap.Bool("saw_cursor", act.SawCursor),
			zap.Bool("moved", act.Moved),
			zap.Bool("blocked", blocked[sid]),
			zap.Bool("eligible", act.Eligible),
			zap.Bool("afk", afk))
		if !afk {
			continue
		}
		afkN++
		if blocked[sid] {
			blockedAfk++
			continue
		}
		if afkFired[sid] {
			alreadyFired++
			continue
		}
		if !act.Eligible {
			ineligible++
			continue
		}
		reason := "no_cursor"
		if act.SawCursor {
			reason = "still"
		}
		flagged++
		afkFired[sid] = true
		out = append(out, CheckResult{SteamID: sid, Type: "afk",
			Detail:  "arming " + reason,
			Message: "This is not an AFK game."})
	}
	e.log.Info("afk_round",
		zap.Int("round", round),
		zap.Int("evaluated", len(ids)),
		zap.Int("afk", afkN),
		zap.Int("flagged", flagged),
		zap.Int("afk_but_blocked", blockedAfk),
		zap.Int("afk_but_ineligible", ineligible),
		zap.Int("afk_but_already_fired", alreadyFired))
	return out
}

// checkBusted is the round-end wire-bot check — the ONLY movement check that reads scoring.
// A player who claimed a scoring slot without EVER sending a cursor the whole round (arming
// and armed) is automation: a human cannot claim a server-RNG'd button without the pointer
// travelling onto it. It reads the WHOLE-ROUND cursor activity (allCursorActivityFn, which
// includes v6 — v6 sends armed cursors, so a real v6 player has SawCursor and won't false-
// fire). blocked players are skipped; independent of afkFired. Never parks (purely punitive).
// Returns results in SteamID order.
func (e *Engine) checkBusted(scored, blocked map[string]bool) []CheckResult {
	if e.allCursorActivityFn == nil {
		return nil
	}
	acts := e.allCursorActivityFn()
	ids := make([]string, 0, len(scored))
	for sid := range scored {
		ids = append(ids, sid)
	}
	sort.Strings(ids)
	var out []CheckResult
	for _, sid := range ids {
		if blocked[sid] {
			continue
		}
		act, ok := acts[sid]
		if !ok || !act.Tracked {
			continue // not judgeable (e.g. legacy) — never busted
		}
		if !act.SawCursor {
			out = append(out, CheckResult{SteamID: sid, Type: "busted",
				Detail:  "scored no_cursor",
				Message: "You know what you did, knock it off."})
		}
	}
	return out
}

// checkLatency is impossible_latency (#2): a scoring click whose arm→click latency is below
// that connection's OWN minimum observed ping RTT is physically impossible (the armed frame
// couldn't have reached the client and a click returned in the time). Per-connection and
// self-calibrating — no global threshold. Gated by ImpossibleLatency>0 and minRTTFn. A
// player with no measured RTT yet (absent from the map) is skipped. blocked players skipped.
func (e *Engine) checkLatency(clicks []ScoredClick, blocked map[string]bool) []CheckResult {
	if e.cfg.ImpossibleLatency <= 0 || e.minRTTFn == nil || len(clicks) == 0 {
		return nil
	}
	rtt := e.minRTTFn()
	// Earliest scoring offset per player (arrival order ⇒ first seen is earliest).
	first := map[string]int{}
	order := []string{}
	for _, c := range clicks {
		if _, seen := first[c.SteamID]; !seen {
			first[c.SteamID] = c.OffsetMs
			order = append(order, c.SteamID)
		}
	}
	var out []CheckResult
	for _, sid := range order {
		if blocked[sid] {
			continue
		}
		minRTT, ok := rtt[sid]
		if !ok || minRTT <= 0 {
			continue
		}
		if first[sid] < minRTT {
			out = append(out, CheckResult{SteamID: sid, Type: "impossible_latency",
				Detail:  fmt.Sprintf("offset=%dms rtt=%dms", first[sid], minRTT),
				Message: "Your click arrived faster than your connection allows."})
		}
	}
	return out
}

// checkTouch is the touch-derived dwell pass (#stretch/#5), gated by TouchCheck>0 and
// touchDataFn. For each scoring click it correlates the claimed button against the player's
// `touch` stamps for this window:
//   - no_hover:   the button was claimed with NO prior touch over it → the pointer never
//                 hovered, near-certain automation (sharper than `busted`, which only sees
//                 "no cursor at all").
//   - fast_hover: touch→click dwell below the human reaction floor (ReactionMinMs) → the
//                 "click" didn't follow a real hover-and-press.
// Only touch-capable players (v7+) are judged — they are the ones PRESENT in touchData (a
// v6/legacy player is absent and exempt). Each type fires at most once per player per round.
// blocked players skipped. Returns results in SteamID order.
func (e *Engine) checkTouch(clicks []ScoredClick, blocked map[string]bool) []CheckResult {
	if e.cfg.TouchCheck <= 0 || e.touchDataFn == nil || len(clicks) == 0 {
		return nil
	}
	touches := e.touchDataFn()
	noHover := map[string]bool{}
	fastHover := map[string]int{} // sid → dwell ms (for the Detail)
	order := []string{}
	seen := map[string]bool{}
	for _, c := range clicks {
		sid := c.SteamID
		if blocked[sid] {
			continue
		}
		bt, judgeable := touches[sid]
		if !judgeable {
			continue // not touch-capable (v6/legacy) — exempt
		}
		if !seen[sid] {
			seen[sid] = true
			order = append(order, sid)
		}
		touchMs, hovered := bt[c.Button]
		if !hovered {
			noHover[sid] = true
			continue
		}
		if e.cfg.ReactionMinMs > 0 {
			if dwell := c.OffsetMs - touchMs; dwell >= 0 && dwell < e.cfg.ReactionMinMs {
				if _, have := fastHover[sid]; !have {
					fastHover[sid] = dwell
				}
			}
		}
	}
	var out []CheckResult
	for _, sid := range order {
		if noHover[sid] {
			out = append(out, CheckResult{SteamID: sid, Type: "no_hover",
				Detail:  "claimed untouched button",
				Message: "You claimed a button your cursor never touched."})
		}
		if d, ok := fastHover[sid]; ok {
			out = append(out, CheckResult{SteamID: sid, Type: "fast_hover",
				Detail:  fmt.Sprintf("dwell=%dms", d),
				Message: "You clicked a button the instant your cursor reached it — too fast to be real."})
		}
	}
	return out
}

// checkStraightPath is straight_path (#6, signal-only, false-positive-prone so OFF unless
// StraightPathRatio>0). It flags a SCORER whose captured cursor path for the window is
// implausibly straight: net displacement / total path length above the ratio across ≥
// StraightPathMin samples. A bot that snaps dead-straight to each button produces a ratio
// near 1; a human hand wavers, lowering it. Reads the same per-window cursor tracks as the
// replay (cursorTracks, still held at round end). blocked players skipped.
func (e *Engine) checkStraightPath(scored, blocked map[string]bool) []CheckResult {
	if e.cfg.StraightPathRatio <= 0 || e.cursorTracksFn == nil {
		return nil
	}
	tracks := e.cursorTracks()
	// tag → steamID is not available here; tracks are keyed by SteamID (hub supplies them
	// keyed by SteamID), so judge scorers directly.
	ids := make([]string, 0, len(scored))
	for sid := range scored {
		ids = append(ids, sid)
	}
	sort.Strings(ids)
	var out []CheckResult
	for _, sid := range ids {
		if blocked[sid] {
			continue
		}
		t, ok := tracks[sid]
		if !ok || len(t.Samples) < e.cfg.StraightPathMin {
			continue
		}
		if ratio, ok := pathStraightness(t.Samples); ok && ratio > e.cfg.StraightPathRatio {
			out = append(out, CheckResult{SteamID: sid, Type: "straight_path",
				Detail:  fmt.Sprintf("ratio=%.3f n=%d", ratio, len(t.Samples)),
				Message: "Your cursor moved in machine-perfect straight lines."})
		}
	}
	return out
}

// coeffVar is the coefficient of variation (stddev/mean) of the gaps between consecutive
// values in offs (offs is in arrival order). Used by the metronome check. Returns false if
// there are too few gaps or the mean is ~0 (no meaningful cadence to judge).
func coeffVar(offs []int) (float64, bool) {
	if len(offs) < 3 {
		return 0, false // need ≥2 gaps for a variance
	}
	gaps := make([]float64, 0, len(offs)-1)
	var sum float64
	for i := 1; i < len(offs); i++ {
		g := float64(offs[i] - offs[i-1])
		gaps = append(gaps, g)
		sum += g
	}
	mean := sum / float64(len(gaps))
	if mean < 1 {
		return 0, false
	}
	var sq float64
	for _, g := range gaps {
		d := g - mean
		sq += d * d
	}
	return math.Sqrt(sq/float64(len(gaps))) / mean, true
}

// pathStraightness is net displacement (start→end) divided by total path length for a
// cursor track. ~1 ⇒ a dead-straight constant-velocity move (a bot snap); a human hand
// wavers, lowering it. Returns false if the total length is ~0 (no real motion to judge).
func pathStraightness(samples []CursorSample) (float64, bool) {
	if len(samples) < 2 {
		return 0, false
	}
	var total float64
	for i := 1; i < len(samples); i++ {
		dx := float64(samples[i].X - samples[i-1].X)
		dy := float64(samples[i].Y - samples[i-1].Y)
		total += math.Hypot(dx, dy)
	}
	if total < 1 {
		return 0, false
	}
	ndx := float64(samples[len(samples)-1].X - samples[0].X)
	ndy := float64(samples[len(samples)-1].Y - samples[0].Y)
	return math.Hypot(ndx, ndy) / total, true
}

// checkSoloSession is the session-level solo_round verdict, evaluated once at the
// end of a game against the set of players who scored in ANY round of it. It flags
// the bounty leader for padding a runaway lead when BOTH:
//
//   - the session was uncontested: the leader is the ONLY player who scored a
//     point in any round (a single scoring click from anyone else makes it
//     contested and the lead stands, however large), and
//   - the leader's lead AFTER this session's win is strictly greater than
//     SoloLeadMargin. leadMargin is the snapshot taken at session start (the gap
//     over the runner-up, or, when the leader is alone on the sessions-won board,
//     their own games-won total). Because an uncontested session means the leader
//     was its sole scorer, they win it and gain exactly one, so the lead this
//     session produces is leadMargin+1. The first lead that fires is therefore
//     SoloLeadMargin+1 (e.g. with the default 4, a resulting lead of 5: entering at
//     4 and winning the 5th uncontested), leaving a newcomer building the board's
//     first wins room to play.
//
// Gating on the leader being the sole scorer also means it never fires on a session
// the leader sat out: if someone else played solo they're the scorer (not leaderID)
// and their own lead won't clear the margin. Returns nil when not flagged.
func (e *Engine) checkSoloSession(scorers map[string]bool, leaderID string, leadMargin int) *CheckResult {
	if e.cfg.SoloLeadMargin <= 0 || leaderID == "" {
		return nil
	}
	// Uncontested: the leader scored and nobody else did.
	if len(scorers) != 1 || !scorers[leaderID] {
		return nil
	}
	// +1: this uncontested session is itself a win for the sole-scoring leader, so
	// the resulting lead is one past the start-of-session snapshot.
	postLead := leadMargin + 1
	if postLead <= e.cfg.SoloLeadMargin { // strictly greater: with the default 4, a lead of 5 fires, 4 doesn't
		return nil
	}
	return &CheckResult{SteamID: leaderID, Type: "solo_round",
		Detail:  fmt.Sprintf("lead=%d", postLead),
		Message: "You're way out front with nobody around to race. Check back when it's busier, or bring some friends for a real game."}
}

// Player-facing lines for the two non-test sanction rungs (the test rung uses the
// triggering check's own Message). The client pairs these with a countdown to the
// frame's until_ms.
const (
	cooldownMsg = "Too many anticheat flags this bounty — you're on a cooldown."
	ignoredMsg  = "You've been sidelined for the rest of this bounty."
)

// bountyInfo returns the active bounty snapshot, or a zero value when no source
// is wired (solo_round and the per-bounty ladder reset then run with id 0).
func (e *Engine) bountyInfo() BountyInfo {
	if e.bountyInfoFn == nil {
		return BountyInfo{}
	}
	return e.bountyInfoFn()
}

// syncBounty aligns the in-memory sanction ladder with the active bounty. It
// always refreshes the cached resolve time (the "ignored" countdown target); when
// the bounty id changes (or on first load) it clears everyone the previous bounty
// sidelined, drops all per-bounty state, and reloads the new bounty's persisted
// sanctions. Called once per game from the Run goroutine.
func (e *Engine) syncBounty(ctx context.Context, bi BountyInfo) {
	e.bountyResolveMs = bi.ResolveAtMs
	if e.bountyLoaded && bi.ID == e.curBountyID {
		return
	}
	if e.bc != nil {
		for sid := range e.underTest {
			e.bc.SendTest(sid, TestFrame{Cleared: true})
		}
		for sid, s := range e.sanctions {
			if s.Ignored || s.CooldownUntil != nil {
				e.bc.SendTest(sid, TestFrame{Cleared: true})
			}
		}
	}
	e.underTest = map[string]bool{}
	e.pendingTests = map[string]pendingTest{}
	e.sanctions = map[string]*Sanction{}
	e.benchMsg = map[string]string{}
	e.notified = map[string]string{}
	e.curBountyID = bi.ID
	e.bountyLoaded = true

	if e.store != nil && bi.ID != 0 {
		loaded, err := e.store.LoadSanctions(ctx, bi.ID)
		if err != nil {
			e.log.Error("load anticheat sanctions", zap.Error(err))
			return
		}
		for sid, s := range loaded {
			sc := s
			e.sanctions[sid] = &sc
		}
	}
	e.publishSanctions()
}

// applyRoundChecks applies the round-end (score-aware) checks with the multi-flag ladder
// boost (#4): a player who trips ≥2 DISTINCT check types in one round is near-certain
// automation, so their ladder is bumped by the number of distinct types that round, not
// just 1. Results are grouped per player; blocked / non-test-capable players are skipped
// from sanctioning (still logged in the round's audit by the caller). The representative
// popup message is the first distinct type's. Deterministic: players processed in SteamID
// order, the message taken from the first check seen for that player.
func (e *Engine) applyRoundChecks(checks []CheckResult, blocked map[string]bool, bi BountyInfo) {
	type agg struct {
		types map[string]bool
		msg   string
	}
	byPlayer := map[string]*agg{}
	order := []string{}
	for _, ch := range checks {
		if blocked[ch.SteamID] {
			continue
		}
		if e.bc == nil || !e.bc.TestCapable(ch.SteamID) {
			continue
		}
		a := byPlayer[ch.SteamID]
		if a == nil {
			a = &agg{types: map[string]bool{}, msg: ch.Message}
			byPlayer[ch.SteamID] = a
			order = append(order, ch.SteamID)
		}
		a.types[ch.Type] = true
	}
	sort.Strings(order)
	for _, sid := range order {
		a := byPlayer[sid]
		e.applySanctionN(sid, len(a.types), a.msg, bi)
	}
}

// applySanction escalates a flagged player's ladder for ONE check (bump 1). Used by the
// arming AFK pass and the session-level solo_round, which raise a single verdict at a time.
func (e *Engine) applySanction(ch CheckResult, bi BountyInfo) {
	e.applySanctionN(ch.SteamID, 1, ch.Message, bi)
}

// applySanctionN escalates a player's ladder by bump (≥1): add to the per-bounty count,
// then place them on the right rung — test (math), a timed cooldown, or ignored-until-the-
// bounty-resolves — and persist the state. The client is (re)notified from notifySanctions
// at the next pending phase. bump>1 is the multi-flag boost (#4).
func (e *Engine) applySanctionN(steamID string, bump int, message string, bi BountyInfo) {
	if bump < 1 {
		bump = 1
	}
	s := e.sanctions[steamID]
	if s == nil {
		s = &Sanction{SteamID: steamID}
		e.sanctions[steamID] = s
	}
	s.Checks += bump

	x := e.cfg.CheckCooldownThreshold
	ignoreAt := x + e.cfg.CheckIgnoreAfter
	now := time.Now()
	switch {
	case x > 0 && s.Checks >= ignoreAt:
		// Past the cooldown plus the grace checks → sidelined for the bounty.
		s.Ignored = true
		e.clearTestRung(steamID)
	case x > 0 && s.CooldownUntil == nil && s.Checks >= x:
		// First time across the threshold → start the timed cooldown.
		until := now.Add(time.Duration(e.cfg.CheckCooldownMins) * time.Minute)
		s.CooldownUntil = &until
		e.clearTestRung(steamID)
	case x > 0 && s.CooldownUntil != nil && now.Before(*s.CooldownUntil):
		// A check landed during an active cooldown (defensive — they're blocked, so
		// this is rare); keep them cooling rather than issuing a test.
		e.clearTestRung(steamID)
	default:
		// Test rung: below the threshold, or post-cooldown grace checks. Bench them
		// behind a math test, remembering which rule fired for the popup.
		e.underTest[steamID] = true
		e.benchMsg[steamID] = message
	}
	e.persistSanction(bi.ID, *s)
	e.publishSanctions()
}

// statusOf is the player-visible rung for a sanction: "ignored", "cooldown" (while
// the cooldown is still running), or "live". The math-test rung isn't a leaderboard
// status — a benched-on-test player still reads as "live" here.
func statusOf(s Sanction) string {
	switch {
	case s.Ignored:
		return "ignored"
	case s.CooldownUntil != nil && time.Now().Before(*s.CooldownUntil):
		return "cooldown"
	default:
		return "live"
	}
}

// liveStatus is the current rung for a player, read from the Run goroutine's own
// maps (no lock). Used when the engine builds standings on that goroutine.
func (e *Engine) liveStatus(steamID string) string {
	if s, ok := e.sanctions[steamID]; ok {
		return statusOf(*s)
	}
	return "live"
}

// annotateStatus stamps each standing with its player's current rung. Called from
// the Run goroutine (reads the live maps via liveStatus), so the session board the
// engine pushes carries status dots too.
func (e *Engine) annotateStatus(standings []Standing) {
	for i := range standings {
		standings[i].Status = e.liveStatus(standings[i].SteamID)
	}
}

// publishSanctions republishes the lock-guarded snapshot from the Run goroutine's
// live state. Call after any mutation so external readers see the change.
func (e *Engine) publishSanctions() {
	snap := make(map[string]Sanction, len(e.sanctions))
	for sid, s := range e.sanctions {
		snap[sid] = *s
	}
	e.sanctionMu.Lock()
	e.sanctionSnap = snap
	e.sanctionMu.Unlock()
}

// SanctionStatuses returns the current non-"live" rung ("cooldown"/"ignored") for
// every sanctioned player, as a fresh map. Safe from any goroutine; used by the API
// to stamp the leaderboard status dots (absent ⇒ "live"). Cooldown expiry is
// evaluated at call time, so an elapsed cooldown reads back "live" without an event.
func (e *Engine) SanctionStatuses() map[string]string {
	e.sanctionMu.RLock()
	snap := e.sanctionSnap
	e.sanctionMu.RUnlock()
	out := make(map[string]string)
	for sid, s := range snap {
		if st := statusOf(s); st != "live" {
			out[sid] = st
		}
	}
	return out
}

// SanctionForChecks derives the full ladder state for a given flag count using the
// live config — the same thresholds applySanction escalates through. Used by the
// admin "set flag count" path so an edit lands the player on the right rung
// immediately (>= threshold → cooldown, >= threshold+grace → ignored) rather than
// only re-escalating on the next real flag. Safe from any goroutine (reads cfg).
func (e *Engine) SanctionForChecks(checks int) Sanction {
	s := Sanction{Checks: checks}
	x := e.cfg.CheckCooldownThreshold
	switch {
	case x > 0 && checks >= x+e.cfg.CheckIgnoreAfter:
		s.Ignored = true
	case x > 0 && checks >= x:
		until := time.Now().Add(time.Duration(e.cfg.CheckCooldownMins) * time.Minute)
		s.CooldownUntil = &until
	}
	return s
}

// applyExternalSanction applies an admin override in the Run goroutine: drop any
// math-test bench for the player, replace (or remove) their sanction state, and
// reconcile what their client is showing. The store row is written by the caller.
func (e *Engine) applyExternalSanction(ev extSanctionEvent) {
	sid := ev.SteamID
	e.clearTestRung(sid)
	if ev.S.Checks <= 0 && !ev.S.Ignored && ev.S.CooldownUntil == nil {
		delete(e.sanctions, sid) // fully forgiven → back to live
	} else {
		s := ev.S
		e.sanctions[sid] = &s
	}
	e.notifySanctions()
	e.publishSanctions()
}

// clearTestRung removes a player from the math-test bench (used when they move to
// a cooldown/ignored rung, where there is no test to answer).
func (e *Engine) clearTestRung(steamID string) {
	delete(e.underTest, steamID)
	delete(e.pendingTests, steamID)
	delete(e.benchMsg, steamID)
}

// persistSanction mirrors a player's ladder state to the store off the Run loop
// (fire-and-forget). No-op without a store or before a bounty is known.
func (e *Engine) persistSanction(bountyID int64, s Sanction) {
	if e.store == nil || bountyID == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := e.store.SaveSanction(ctx, bountyID, s); err != nil {
			e.log.Error("persist anticheat sanction", zap.Error(err))
		}
	}()
}

// blockedMap snapshots every player who can't score this round: those on the math
// test, in an active cooldown, or ignored for the bounty. Used to size N and to
// withhold the armed nonce. nil when nobody is blocked (the common case).
func (e *Engine) blockedMap() map[string]bool {
	now := time.Now()
	out := map[string]bool{}
	for sid := range e.underTest {
		out[sid] = true
	}
	for sid, s := range e.sanctions {
		if s.Ignored || (s.CooldownUntil != nil && now.Before(*s.CooldownUntil)) {
			out[sid] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// notifySanctions reconciles what each sanctioned player's client is showing with
// their current rung: (re)send the math test to test-rung players, the cooldown /
// ignored countdown to those on those rungs (only to clients that can render them),
// and a clear to anyone previously notified who is now back in play. Called
// each pending phase, so countdowns refresh and reconnecting clients catch up.
func (e *Engine) notifySanctions() {
	if e.bc == nil {
		return
	}
	now := time.Now()

	for sid := range e.underTest {
		pt, ok := e.pendingTests[sid]
		if !ok {
			pt = e.newTest(sid)
			e.pendingTests[sid] = pt
		}
		e.bc.SendTest(sid, TestFrame{State: "test", ID: pt.id, Kind: pt.kind, Prompt: pt.prompt, Message: e.benchMsg[sid]})
		e.notified[sid] = "test"
	}

	for sid, s := range e.sanctions {
		switch {
		case s.Ignored:
			if e.bc.SanctionCapable(sid) {
				e.bc.SendTest(sid, TestFrame{State: "ignored", UntilMs: e.bountyResolveMs, Message: ignoredMsg})
			}
			e.notified[sid] = "ignored"
		case s.CooldownUntil != nil && now.Before(*s.CooldownUntil):
			if e.bc.SanctionCapable(sid) {
				e.bc.SendTest(sid, TestFrame{State: "cooldown", UntilMs: s.CooldownUntil.UnixMilli(), Message: cooldownMsg})
			}
			e.notified[sid] = "cooldown"
		}
	}

	// Anyone we previously notified who is no longer blocked (passed their test, or a
	// cooldown elapsed) gets a clear so their overlay disappears.
	for sid, st := range e.notified {
		if st == "" {
			continue
		}
		if e.underTest[sid] {
			continue
		}
		if s, ok := e.sanctions[sid]; ok && (s.Ignored || (s.CooldownUntil != nil && now.Before(*s.CooldownUntil))) {
			continue
		}
		e.bc.SendTest(sid, TestFrame{Cleared: true})
		delete(e.notified, sid)
	}
}

// handleAnswer settles a benched player's test answer. A correct answer clears
// the bench (they rejoin at the next arm); a wrong one is recorded and a fresh
// test issued. Answers that don't match the outstanding test id are ignored
// (stale/duplicate). Both outcomes are persisted for the audit trail.
func (e *Engine) handleAnswer(a answerEvent) {
	pt, ok := e.pendingTests[a.SteamID]
	if !ok || pt.id != a.ID {
		return
	}
	correct := strings.TrimSpace(a.Answer) == pt.expected
	e.recordTestAnswer(pt.id, a.Answer, correct)
	if correct {
		delete(e.pendingTests, a.SteamID)
		delete(e.underTest, a.SteamID)
		delete(e.benchMsg, a.SteamID)
		delete(e.notified, a.SteamID)
		if e.bc != nil {
			e.bc.SendTest(a.SteamID, TestFrame{Cleared: true})
		}
		return
	}
	// Wrong: issue a fresh test so they can't brute-force the same prompt.
	npt := e.newTest(a.SteamID)
	e.pendingTests[a.SteamID] = npt
	if e.bc != nil {
		e.bc.SendTest(a.SteamID, TestFrame{State: "test", ID: npt.id, Kind: npt.kind, Prompt: npt.prompt, Message: e.benchMsg[a.SteamID]})
	}
}

func recordInfo(info map[string]playerInfo, ev ClickEvent) {
	info[ev.SteamID] = playerInfo{tag: ev.Tag, username: ev.Username}
}

// standingsOf orders players by points (desc). Ties are broken by who reached
// their current total FIRST: reached[sid] is the arrival time of the click that
// brought that player to their score, so the earlier timestamp ranks higher.
// SteamID is a final, stable fallback (missing/equal timestamps — e.g. a player
// with no recorded scoring click).
func standingsOf(scores map[string]int, info map[string]playerInfo, reached map[string]time.Time) []Standing {
	out := make([]Standing, 0, len(scores))
	for sid, pts := range scores {
		pi := info[sid]
		out = append(out, Standing{Tag: pi.tag, Username: pi.username, Points: pts, SteamID: sid})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Points != out[j].Points {
			return out[i].Points > out[j].Points
		}
		ti, tj := reached[out[i].SteamID], reached[out[j].SteamID]
		if !ti.Equal(tj) {
			return ti.Before(tj) // reached the tied total earlier ⇒ ranks first
		}
		return out[i].SteamID < out[j].SteamID
	})
	return out
}

// stampBehindMs annotates a SORTED standings slice with each tied player's gap to
// the player directly above them on the same score: the ms between their
// tie-deciding clicks ("how much you lost the game by"). It's an end-of-game
// notion — the per-round running standings have transient ties, so this is stamped
// only on the final game standings, not on round_result. Top of a tie group / a
// unique score is left at 0 (omitempty drops it on the wire).
func stampBehindMs(standings []Standing, reached map[string]time.Time) {
	for i := 1; i < len(standings); i++ {
		if standings[i].Points != standings[i-1].Points {
			continue
		}
		prev, cur := reached[standings[i-1].SteamID], reached[standings[i].SteamID]
		if prev.IsZero() || cur.IsZero() {
			continue // missing timing (e.g. no recorded scoring click) — no gap shown
		}
		if ms := cur.Sub(prev).Milliseconds(); ms > 0 {
			standings[i].BehindMs = int(ms)
		}
	}
}

func topK(s []Standing, k int) []Standing {
	if k > 0 && len(s) > k {
		return s[:k]
	}
	return s
}

func newNonce() uint64 {
	var b [8]byte
	cryptorand.Read(b[:])
	n := binary.BigEndian.Uint64(b[:])
	if n == 0 {
		n = 1 // 0 is the "no nonce" sentinel — never issue it as a real nonce
	}
	return n
}

func newID() string {
	var b [16]byte
	cryptorand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func seedFromCrypto() int64 {
	var b [8]byte
	cryptorand.Read(b[:])
	return int64(binary.BigEndian.Uint64(b[:]))
}
