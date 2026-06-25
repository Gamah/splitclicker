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
	// players' screens. HasPos is false when the client sent no position (an older,
	// below-v5 build): such clicks still score but are omitted from the pip sample.
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

// ArmedFrame goes live: the race is open. Penalties is keyed by SteamID — the
// hub holds back that connection's copy of the armed frame by the given delay
// (the spam deterrent) and echoes that same delay back so the player can see
// they're being throttled. Nonce must be echoed by a scoring click.
type ArmedFrame struct {
	Round int
	Seq   int
	// Nonce is the persistent legacy button echoed by below-v5 ("v4") scoring clicks
	// (a single button valid the whole window). Buttons is the initial board of live
	// buttons for v5+ clients (each {SlotID, Nonce, X, Y}); the hub sends Buttons to
	// tick-capable clients and Nonce to the rest. See board.legacyNonce.
	Nonce     uint64
	Buttons   []Button
	Players   int
	Clicks    int
	Penalties map[string]time.Duration
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
	// SanctionCapable reports whether the client understands the v4 sanction states
	// (cooldown / ignored countdowns). Such a client is sidelined silently when on a
	// non-test rung — it still can't score, it just doesn't get a frame it can't
	// render. False if not connected.
	SanctionCapable(steamID string) bool
}

// --- store (the persistent hourly board) ---

// HourlyDelta is a points increment for one player.
type HourlyDelta struct {
	SteamID string
	Points  int
}

// ScoredClick is one click that took a scoring slot in a round. SlotNo is the
// "click N" (0-based arrival order); OffsetMs is its wire-arrival latency
// measured from the arm (the click's At minus the round's armed_at).
type ScoredClick struct {
	SteamID  string
	SlotNo   int
	OffsetMs int
}

// CursorActivity is a scoring player's mouse activity during the round just
// played, supplied by the ws hub (which tracks a per-window cursor bounding box).
// Tracked is false for a connection that can't be judged — a below-v5 (legacy)
// client never sends cursors, or the player isn't currently connected — so the
// afk_score check skips them rather than flagging them. SawCursor is whether any
// cursor message arrived this window (false ⇒ tabbed out / idle pointer). Extent is
// the Manhattan span of the cursor's bounding box in normalized int16 units (0 =
// never moved).
type CursorActivity struct {
	Tracked   bool
	SawCursor bool
	Extent    int
}

// CheckResult is one anticheat check a round flagged against a player. Type is
// the rule that fired ('fast_clicks' | 'too_many_clicks' | 'solo_round' |
// 'dominant_winner' | 'afk_score'); Detail is a short note ('delta=84ms' / 'clicks=37'
// / 'clicks=12 vs 5' / 'no_cursor' / 'extent=120 min=1000') for the audit record;
// Message is the player-facing line shown in the anticheat popup explaining which rule
// benched them.
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
	// so it needs no lock. It is read and reset at each arm to delay that connection's
	// armed frame (the spam deterrent); see idlePenalty.
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
	// time bounds the "ignored" rung). Optional — nil disables solo_round and the
	// ladder runs with a zero bounty id (counts never reset, no resolve-time
	// countdown). Read from the in-memory leaderboard cache, so it's cheap to call.
	bountyInfoFn func() BountyInfo

	// cursorActivityFn, if set, returns a scoring player's cursor activity during the
	// round just played (whether they sent any cursor messages, and how far the cursor
	// roamed). Used by the afk_score check (see afkByCursor) to flag a player who took a
	// scoring slot while AFK by the cursor. Supplied by the ws hub, which tracks the
	// per-window cursor bounding box. Optional — nil disables afk_score.
	cursorActivityFn func(steamID string) CursorActivity

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

// SetCursorActivityFn registers the source of per-player cursor activity (used by
// the afk check). Call before Run; nil disables afk. See CursorActivity.
func (e *Engine) SetCursorActivityFn(fn func(steamID string) CursorActivity) { e.cursorActivityFn = fn }

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
	// Bad-click penalty escalation, sent on connect so the client mirrors the live
	// throttle estimate without hardcoding the formula (see idlePenalty).
	PenaltyBaseMs int
	PenaltyStepMs int
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
		PenaltyBaseMs: e.cfg.PenaltyBaseMs, PenaltyStepMs: e.cfg.PenaltyStepMs,
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
	for round := 1; round <= x && ctx.Err() == nil; round++ {
		// players is the full connected crowd (shown to clients + recorded); N is sized
		// to the players who can actually race — benched/cooled-down/ignored players are
		// excluded so they don't inflate the scoring slots.
		players := e.bc.PlayerCount()
		active := e.bc.ActivePlayerCount(e.blockedMap())
		n := e.clicksFor(active)
		e.pending(ctx, round, x, players, n, info)
		if ctx.Err() != nil {
			return
		}
		final := round == x
		deltas, roundID, clicks, armedAt := e.race(ctx, round, x, players, n, scores, info, reached, final)
		if ctx.Err() != nil {
			return
		}
		// Run the end-of-round anticheat checks against this round's scoring clicks.
		// Every flagged check is logged; applySanction then escalates the ladder for
		// test-capable players (test → cooldown → ignored) and pushes them a frame.
		checks := e.runChecks(clicks, checkCtx{n: n})
		for _, ch := range checks {
			if e.bc != nil && e.bc.TestCapable(ch.SteamID) {
				e.applySanction(ch, bi)
			}
		}
		for _, c := range clicks {
			sessionScorers[c.SteamID] = true
		}
		roundLogs = append(roundLogs, RoundLog{
			RoundID: roundID, RoundNo: round, N: n, Players: players,
			ArmedAt: armedAt, Clicks: clicks, Checks: checks,
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
	if ch := e.checkSoloSession(sessionScorers, bi.LeaderID, bi.LeadMargin); ch != nil {
		if e.bc != nil && e.bc.TestCapable(ch.SteamID) {
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

// pending is the IDLE/arming phase: announce the round, then wait the secret
// delay. Idle clicks during it score nothing but accrue this connection's
// cross-phase bad-click tally (e.badClicks), applied as an arm-delay penalty at
// the next arm.
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
func (e *Engine) race(ctx context.Context, round, of, players, n int, scores map[string]int, info map[string]playerInfo, reached map[string]time.Time, final bool) (map[string]int, string, []ScoredClick, time.Time) {
	legacyNonce := newNonce()
	penalties := e.penaltiesFrom(e.badClicks)
	blocked := e.blockedMap()
	e.setPhase(PhaseArmed, round)
	armedAt := time.Now()

	// Build the live board: mint up to ButtonsOnScreen buttons, each a fresh nonce at a
	// non-overlapping server-RNG'd position. The initial set rides the armed frame to v5
	// clients; legacyNonce is the single persistent button below-v5 clients click. mint
	// also refills the board after each claim (see board.offer).
	b := newBoard(n, legacyNonce, armedAt)
	b.mint = func() Button {
		x, y := e.randPos(b.positions())
		return b.register(newNonce(), x, y)
	}
	buttons := make([]Button, 0, e.cfg.ButtonsOnScreen)
	for i := 0; i < e.cfg.ButtonsOnScreen; i++ {
		buttons = append(buttons, b.mint())
	}

	e.bc.Armed(ArmedFrame{Round: round, Seq: round, Nonce: legacyNonce, Buttons: buttons, Players: players, Clicks: n, Penalties: penalties, Blocked: blocked})
	// Each arm forgives the bad clicks accrued since the previous arm: the penalty
	// above already reflects them, and the tally now restarts for the next arm.
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
			return nil, "", nil, armedAt
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

	deltas := map[string]int{}
	clicks := make([]ScoredClick, len(b.scored))
	for i, ev := range b.scored {
		deltas[ev.SteamID]++
		scores[ev.SteamID]++
		// Record the arrival time of each scoring click; the last one wins, so
		// reached[sid] ends up as when this player reached their current total —
		// the tie-break standingsOf uses ("who got there first").
		reached[ev.SteamID] = ev.At
		clicks[i] = ScoredClick{SteamID: ev.SteamID, SlotNo: i, OffsetMs: int(ev.At.Sub(armedAt).Milliseconds())}
	}
	if len(deltas) > 0 && e.store != nil {
		e.persist(deltas)
	}
	roundID := newID()

	// Final round: no separate round_result or result-display pause — playGame
	// emits game_over next, carrying these deltas/roundID.
	if final {
		return deltas, roundID, clicks, armedAt
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
	return deltas, roundID, clicks, armedAt
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

// idlePenalty is the accumulated arm-delay penalty after n bad clicks accrued
// since the last arm: sum_{k=1..n}(base + step·(k−1)) = base·n + step·n(n−1)/2,
// where base/step are the configured PenaltyBaseMs/PenaltyStepMs (default
// 500/100 → totals 500,1100,1800,2600… ms).
func (e *Engine) idlePenalty(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	ms := e.cfg.PenaltyBaseMs*n + e.cfg.PenaltyStepMs*n*(n-1)/2
	return time.Duration(ms) * time.Millisecond
}

// penaltiesFrom turns the accrued bad-click tally into per-connection arm-delay
// penalties for the hub to hold back. nil when nobody's earned one (the common case).
func (e *Engine) penaltiesFrom(bad map[string]int) map[string]time.Duration {
	if len(bad) == 0 {
		return nil
	}
	out := make(map[string]time.Duration, len(bad))
	for sid, n := range bad {
		out[sid] = e.idlePenalty(n)
	}
	return out
}

// recordBad bumps a connection's cross-phase bad-click tally for an idle (zero-
// nonce) click — one that can never score in any phase. Non-zero-nonce clicks are
// race attempts (handled by the race), never penalised even when they lose.
func (e *Engine) recordBad(ev ClickEvent) {
	if ev.Nonce == 0 {
		e.badClicks[ev.SteamID]++
	}
}

// --- anticheat: checks + tests ---

// checkCtx is the round context the per-round checks need beyond the scoring
// clicks: n, the round's scoring-slot count. The too_many_clicks divisor is
// derived from the scoring clicks themselves (how many players actually scored
// this round), not from a connection count. (solo_round is NOT a per-round check —
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
//   - afk_score:       took a scoring slot while AFK by the cursor (afkByCursor: no
//                      cursor messages this round, or the pointer never moved past
//                      AfkCursorMin from its round-start spot). The "gotcha" — buttons
//                      spawn at server-RNG'd spots, so scoring without the cursor
//                      travelling is the bot signature. Per-player (no ≥2-scorer gate),
//                      so it catches a lone automated clicker; needs cursorActivityFn +
//                      AfkCursorMin>0 and skips players it can't judge (legacy clients
//                      send no cursors; a disconnected player has no activity). Only
//                      scorers are judged — moving but missing buttons is fine.
//
// solo_round is deliberately absent here — it's a session-level verdict (was the
// WHOLE session uncontested?), evaluated once at session end by checkSoloSession.
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
		// too_many_clicks: an implausible share of this round's slots.
		if limit > 0 && len(offs) > limit {
			out = append(out, CheckResult{SteamID: sid, Type: "too_many_clicks",
				Detail:  fmt.Sprintf("clicks=%d limit=%d", len(offs), limit),
				Message: "You took far more of the round's clicks than your share."})
		}
		// afk_score (the "gotcha"): this player took a SCORING slot while AFK by the
		// cursor (see afkByCursor — no cursor messages this round, or the pointer never
		// moved meaningfully from where it sat when the round began). Buttons spawn at
		// server-RNG'd spots, so a real player's cursor travels to reach them; claiming
		// one without that is the bot signature. Only scoring players are judged — moving
		// around and MISSING buttons (no score) is fine — and legacy/disconnected players
		// are skipped (not tracked). The message stays vague: it must NOT reveal that
		// cursor movement is what's measured. Detail records which half of the AFK check
		// fired, for the audit trail.
		if e.cfg.AfkCursorMin > 0 && e.cursorActivityFn != nil {
			if act := e.cursorActivityFn(sid); act.Tracked && afkByCursor(act, e.cfg.AfkCursorMin) {
				detail := "no_cursor"
				if act.SawCursor {
					detail = fmt.Sprintf("extent=%d min=%d", act.Extent, e.cfg.AfkCursorMin)
				}
				out = append(out, CheckResult{SteamID: sid, Type: "afk_score",
					Detail:  detail,
					Message: "This is not an AFK game."})
			}
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

// afkByCursor is the AFK check: a (tracked) player is AFK this round if no cursor
// messages arrived at all, or the cursor never moved meaningfully from its round-start
// position — its bounding-box span over the round stayed below min. The afk_score
// "gotcha" fires when a player who is AFK by this check nonetheless took a scoring slot.
func afkByCursor(act CursorActivity, min int) bool {
	return !act.SawCursor || act.Extent < min
}

// checkSoloSession is the session-level solo_round verdict, evaluated once at the
// end of a game against the set of players who scored in ANY round of it. It flags
// the bounty leader for padding a runaway lead when BOTH:
//
//   - the session was uncontested — the leader is the ONLY player who scored a
//     point in any round (a single scoring click from anyone else makes it
//     contested and the lead stands, however large), and
//   - the leader's lead AFTER this session's win is strictly greater than
//     SoloLeadMargin. leadMargin is the snapshot taken at session start (the gap
//     over the runner-up, or, when the leader is alone on the sessions-won board,
//     their own games-won total). Because an uncontested session means the leader
//     was its sole scorer, they win it and gain exactly one — so the lead this
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

// applySanction escalates a flagged player's ladder for one check: bump the
// per-bounty count, then place them on the right rung — test (math), a timed
// cooldown, or ignored-until-the-bounty-resolves — and persist the state. The
// client is (re)notified from notifySanctions at the next pending phase.
func (e *Engine) applySanction(ch CheckResult, bi BountyInfo) {
	s := e.sanctions[ch.SteamID]
	if s == nil {
		s = &Sanction{SteamID: ch.SteamID}
		e.sanctions[ch.SteamID] = s
	}
	s.Checks++

	x := e.cfg.CheckCooldownThreshold
	ignoreAt := x + e.cfg.CheckIgnoreAfter
	now := time.Now()
	switch {
	case x > 0 && s.Checks >= ignoreAt:
		// Past the cooldown plus the grace checks → sidelined for the bounty.
		s.Ignored = true
		e.clearTestRung(ch.SteamID)
	case x > 0 && s.CooldownUntil == nil && s.Checks >= x:
		// First time across the threshold → start the timed cooldown.
		until := now.Add(time.Duration(e.cfg.CheckCooldownMins) * time.Minute)
		s.CooldownUntil = &until
		e.clearTestRung(ch.SteamID)
	case x > 0 && s.CooldownUntil != nil && now.Before(*s.CooldownUntil):
		// A check landed during an active cooldown (defensive — they're blocked, so
		// this is rare); keep them cooling rather than issuing a test.
		e.clearTestRung(ch.SteamID)
	default:
		// Test rung: below the threshold, or post-cooldown grace checks. Bench them
		// behind a math test, remembering which rule fired for the popup.
		e.underTest[ch.SteamID] = true
		e.benchMsg[ch.SteamID] = ch.Message
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
// ignored countdown to those on those rungs (only to v4 clients that can render
// them), and a clear to anyone previously notified who is now back in play. Called
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
