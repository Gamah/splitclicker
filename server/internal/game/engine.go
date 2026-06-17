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
}

// Standing is one player's position on a board. SteamID64 is public information
// (it is literally the public Steam-profile identifier), so it is sent to clients
// — the UI uses it to open/copy a player's steamcommunity.com profile.
type Standing struct {
	Tag      string `json:"tag"`
	Username string `json:"username"`
	Points   int    `json:"points"`
	SteamID  string `json:"steam_id"`
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
	Round     int
	Seq       int
	Nonce     uint64
	Players   int
	Clicks    int
	Penalties map[string]time.Duration
}

// ResultFrame is the post-round leaderboard. Deltas is per-SteamID points scored
// this round; the hub merges each connection's own delta + RoundID into its copy
// so the client can drive its `points` achievement stat exactly once.
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

// Broadcaster is how the engine reaches connected clients. All methods are
// called from the engine's single Run goroutine, except PlayerCount which must
// be safe from any goroutine (the engine reads it to size each round's race).
type Broadcaster interface {
	Pending(PendingFrame)
	Armed(ArmedFrame)
	Result(ResultFrame)
	GameOver(GameOverFrame)
	// DevNote pushes the current host-editable broadcast note to every client
	// (empty string clears it). Sent once per game.
	DevNote(note string)
	PlayerCount() int
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

// RoundLog is the durable record of one round: its identity, parameters, arm
// time, and the scoring clicks in arrival order.
type RoundLog struct {
	RoundID string
	RoundNo int
	N       int
	Players int
	ArmedAt time.Time
	Clicks  []ScoredClick
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

	// gameEndHook, if set, runs after each game_over (post session-win write);
	// used to refresh the leaderboard cache once per "session". Optional.
	gameEndHook func(context.Context)

	// devNoteFn, if set, supplies the host-editable broadcast note; it is called
	// once at the start of each game (so config edits apply on the next game).
	// Optional — nil means no note is ever sent. Call SetDevNoteFn before Run.
	devNoteFn func() string
}

// SetGameEndHook registers a callback run after every game_over, once the
// session win has been persisted. Call before Run; nil clears it.
func (e *Engine) SetGameEndHook(fn func(context.Context)) { e.gameEndHook = fn }

// SetDevNoteFn registers the source of the per-game dev note. Call before Run;
// nil clears it.
func (e *Engine) SetDevNoteFn(fn func() string) { e.devNoteFn = fn }

// New builds an Engine. store may be nil (scoring still works, just not
// persisted). log may be nil (falls back to a no-op logger).
func New(cfg Config, bc Broadcaster, store Store, log *zap.Logger) *Engine {
	if log == nil {
		log = zap.NewNop()
	}
	return &Engine{
		cfg:    cfg,
		bc:     bc,
		store:  store,
		log:    log,
		clicks:    make(chan ClickEvent, 4096),
		wake:      make(chan struct{}, 1),
		rng:       rand.New(rand.NewSource(seedFromCrypto())),
		phase:     PhaseIntermission,
		badClicks: map[string]int{},
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
	}
}

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
		}
	}
	return true
}

func (e *Engine) playGame(ctx context.Context) {
	gameID := newID()
	startedAt := time.Now().UTC()
	scores := map[string]int{}      // cumulative game points by SteamID
	info := map[string]playerInfo{} // latest display info by SteamID
	roundLogs := []RoundLog{}       // durable per-round history, flushed at game end
	x := e.cfg.RoundsPerGame

	// Refresh the host-editable broadcast note once per game and push it to every
	// client (empty clears it on the client — "shows until an empty note is sent").
	note := ""
	if e.devNoteFn != nil {
		note = e.devNoteFn()
	}
	e.setDevNote(note)
	e.bc.DevNote(note)

	// The final round's points + round id, carried into game_over (the last round
	// has no separate round_result — see race/final).
	var finalDeltas map[string]int
	var finalRoundID string
	for round := 1; round <= x && ctx.Err() == nil; round++ {
		// Size the round to the crowd at arm time: N scales with connected players.
		players := e.bc.PlayerCount()
		n := e.clicksFor(players)
		e.pending(ctx, round, x, players, n, info)
		if ctx.Err() != nil {
			return
		}
		final := round == x
		deltas, roundID, clicks, armedAt := e.race(ctx, round, x, players, n, scores, info, final)
		if ctx.Err() != nil {
			return
		}
		roundLogs = append(roundLogs, RoundLog{
			RoundID: roundID, RoundNo: round, N: n, Players: players,
			ArmedAt: armedAt, Clicks: clicks,
		})
		if final {
			finalDeltas, finalRoundID = deltas, roundID
		}
	}
	if ctx.Err() != nil {
		return
	}

	final := standingsOf(scores, info)
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

	timer := time.NewTimer(e.randArmDelay())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-e.clicks:
			recordInfo(info, ev)
			e.recordBad(ev)
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
func (e *Engine) race(ctx context.Context, round, of, players, n int, scores map[string]int, info map[string]playerInfo, final bool) (map[string]int, string, []ScoredClick, time.Time) {
	nonce := newNonce()
	penalties := e.penaltiesFrom(e.badClicks)
	e.setPhase(PhaseArmed, round)
	armedAt := time.Now()
	e.bc.Armed(ArmedFrame{Round: round, Seq: round, Nonce: nonce, Players: players, Clicks: n, Penalties: penalties})
	// Each arm forgives the bad clicks accrued since the previous arm: the penalty
	// above already reflects them, and the tally now restarts for the next arm.
	e.badClicks = map[string]int{}

	rs := newRaceState(nonce, n)
	timer := time.NewTimer(e.cfg.RaceMax)
	defer timer.Stop()

raceLoop:
	for !rs.full() {
		select {
		case <-ctx.Done():
			return nil, "", nil, armedAt
		case ev := <-e.clicks:
			recordInfo(info, ev)
			if !rs.offer(ev) {
				e.recordBad(ev) // an idle click during the live window still penalises
			}
		case <-timer.C:
			break raceLoop // safety: fewer than N clicks arrived
		}
	}

	deltas := map[string]int{}
	clicks := make([]ScoredClick, len(rs.scored))
	for i, ev := range rs.scored {
		deltas[ev.SteamID]++
		scores[ev.SteamID]++
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
	// several slots still shows once, with their cumulative game points.
	winners := make([]Standing, 0, len(deltas))
	seen := make(map[string]bool, len(deltas))
	for _, ev := range rs.scored {
		if seen[ev.SteamID] {
			continue
		}
		seen[ev.SteamID] = true
		pi := info[ev.SteamID]
		winners = append(winners, Standing{Tag: pi.tag, Username: pi.username, Points: scores[ev.SteamID], SteamID: ev.SteamID})
	}
	e.bc.Result(ResultFrame{
		Round:     round,
		Of:        of,
		Winners:   winners,
		Standings: topK(standingsOf(scores, info), e.cfg.BoardSize),
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

// raceState accepts the first N valid clicks for one arm. It is the whole
// scoring rule in one place: nonce gating (anti-pre-fire) and the hard N cutoff.
// The window stays open until N clicks are consumed (or RaceMax), and a single
// player may take multiple slots — repeated clicks from the same player each
// score, so a fast clicker is rewarded for mashing inside the live window.
type raceState struct {
	nonce  uint64
	n      int
	scored []ClickEvent
}

func newRaceState(nonce uint64, n int) *raceState {
	if n < 1 {
		n = 1
	}
	return &raceState{nonce: nonce, n: n}
}

func (rs *raceState) full() bool { return len(rs.scored) >= rs.n }

// offer reports whether ev scored. A wrong/zero nonce (pre-fire or stale) or a
// full race scores nothing; otherwise the click takes the next of the N slots.
func (rs *raceState) offer(ev ClickEvent) bool {
	if rs.full() {
		return false
	}
	if ev.Nonce != rs.nonce {
		return false
	}
	rs.scored = append(rs.scored, ev)
	return true
}

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

func recordInfo(info map[string]playerInfo, ev ClickEvent) {
	info[ev.SteamID] = playerInfo{tag: ev.Tag, username: ev.Username}
}

// standingsOf builds the full board sorted by points desc, SteamID asc as a
// stable tiebreak (so placements are deterministic).
func standingsOf(scores map[string]int, info map[string]playerInfo) []Standing {
	out := make([]Standing, 0, len(scores))
	for sid, pts := range scores {
		pi := info[sid]
		out = append(out, Standing{Tag: pi.tag, Username: pi.username, Points: pts, SteamID: sid})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Points != out[j].Points {
			return out[i].Points > out[j].Points
		}
		return out[i].SteamID < out[j].SteamID
	})
	return out
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
