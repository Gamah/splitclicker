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
// PenaltyPerClickMs/PenaltyCapMs are the (static) spam-penalty tunables, sent so
// the client can count its own idle-click throttle live this round — the server's
// authoritative value still overwrites it in the armed frame.
type PendingFrame struct {
	Round             int
	Of                int
	Players           int
	Clicks            int
	PenaltyPerClickMs int
	PenaltyCapMs      int
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
type GameOverFrame struct {
	Standings  []Standing
	GameID     string
	Placements map[string]int
	Won        map[string]bool
}

// Broadcaster is how the engine reaches connected clients. All methods are
// called from the engine's single Run goroutine, except PlayerCount which must
// be safe from any goroutine (the engine reads it to size each round's race).
type Broadcaster interface {
	Pending(PendingFrame)
	Armed(ArmedFrame)
	Result(ResultFrame)
	GameOver(GameOverFrame)
	PlayerCount() int
}

// --- store (the persistent hourly board) ---

// HourlyDelta is a points increment for one player.
type HourlyDelta struct {
	SteamID string
	Points  int
}

// Store persists scoring. bucket is the UTC clock-hour the points belong to.
// AddSessionWin credits one game ("session") win to the steamID that topped a
// completed game's final standings.
type Store interface {
	AddHourlyPoints(ctx context.Context, bucket time.Time, deltas []HourlyDelta) error
	AddSessionWin(ctx context.Context, steamID string) error
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

	rngMu sync.Mutex
	rng   *rand.Rand

	mu    sync.RWMutex // guards phase/round (read by Snapshot)
	phase Phase
	round int
}

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
		clicks: make(chan ClickEvent, 4096),
		rng:    rand.New(rand.NewSource(seedFromCrypto())),
		phase:  PhaseIntermission,
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
	Phase             Phase
	Round             int
	Of                int
	Players           int
	Clicks            int
	ArmMinSec         int // arming-window bounds (the per-round delay itself stays secret)
	ArmMaxSec         int
	PenaltyPerClickMs int // spam-penalty tunables, so a client can count its own throttle
	PenaltyCapMs      int
}

func (e *Engine) Snapshot() Snapshot {
	e.mu.RLock()
	phase, round := e.phase, e.round
	e.mu.RUnlock()
	players := 0
	if e.bc != nil {
		players = e.bc.PlayerCount()
	}
	return Snapshot{
		Phase: phase, Round: round, Of: e.cfg.RoundsPerGame,
		Players: players, Clicks: e.clicksFor(players),
		ArmMinSec: int(e.cfg.ArmMin / time.Second), ArmMaxSec: int(e.cfg.ArmMax / time.Second),
		PenaltyPerClickMs: int(e.cfg.IdlePenaltyPerClick / time.Millisecond),
		PenaltyCapMs:      int(e.cfg.IdlePenaltyCap / time.Millisecond),
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

// Run drives games until ctx is cancelled. Blocks; call in its own goroutine.
func (e *Engine) Run(ctx context.Context) {
	for ctx.Err() == nil {
		e.playGame(ctx)
		if ctx.Err() != nil {
			return
		}
		e.setPhase(PhaseIntermission, 0)
		e.drain(ctx, e.cfg.Intermission)
	}
}

func (e *Engine) playGame(ctx context.Context) {
	gameID := newID()
	scores := map[string]int{}      // cumulative game points by SteamID
	info := map[string]playerInfo{} // latest display info by SteamID
	x := e.cfg.RoundsPerGame

	for round := 1; round <= x && ctx.Err() == nil; round++ {
		// Size the round to the crowd at arm time: N scales with connected players.
		players := e.bc.PlayerCount()
		n := e.clicksFor(players)
		penalties := e.pending(ctx, round, x, players, n, info)
		if ctx.Err() != nil {
			return
		}
		e.race(ctx, round, x, players, n, penalties, scores, info)
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
	})

	// Credit the session win to the game's top scorer (if anyone scored), off the
	// hot path so DB latency never delays the intermission.
	if len(final) > 0 && e.store != nil {
		e.creditSessionWin(final[0].SteamID)
	}
}

// creditSessionWin records a game win for steamID on the persistent "sessions
// won" board. Detached context so a shutdown right after game_over still records.
func (e *Engine) creditSessionWin(steamID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := e.store.AddSessionWin(ctx, steamID); err != nil {
			e.log.Error("persist session win", zap.Error(err))
		}
	}()
}

// pending is the IDLE/arming phase: announce the round, then wait the secret
// delay. Clicks during this phase score nothing but accrue a per-connection arm
// delay (the spam deterrent), returned keyed by SteamID for the hub to apply.
func (e *Engine) pending(ctx context.Context, round, of, players, n int, info map[string]playerInfo) map[string]time.Duration {
	e.setPhase(PhasePending, round)
	e.bc.Pending(PendingFrame{
		Round: round, Of: of, Players: players, Clicks: n,
		PenaltyPerClickMs: int(e.cfg.IdlePenaltyPerClick / time.Millisecond),
		PenaltyCapMs:      int(e.cfg.IdlePenaltyCap / time.Millisecond),
	})

	penalties := map[string]time.Duration{}
	timer := time.NewTimer(e.randArmDelay())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return penalties
		case ev := <-e.clicks:
			recordInfo(info, ev)
			penalties[ev.SteamID] = clampPenalty(penalties[ev.SteamID], e.cfg.IdlePenaltyPerClick, e.cfg.IdlePenaltyCap)
		case <-timer.C:
			return penalties
		}
	}
}

// race is the ARMED phase: arm with a fresh nonce, accept the first N valid
// clicks (by arrival), then publish the result. Closes the instant click N lands.
func (e *Engine) race(ctx context.Context, round, of, players, n int, penalties map[string]time.Duration, scores map[string]int, info map[string]playerInfo) {
	nonce := newNonce()
	e.setPhase(PhaseArmed, round)
	e.bc.Armed(ArmedFrame{Round: round, Seq: round, Nonce: nonce, Players: players, Clicks: n, Penalties: penalties})

	rs := newRaceState(nonce, n)
	timer := time.NewTimer(e.cfg.RaceMax)
	defer timer.Stop()

raceLoop:
	for !rs.full() {
		select {
		case <-ctx.Done():
			return
		case ev := <-e.clicks:
			recordInfo(info, ev)
			rs.offer(ev)
		case <-timer.C:
			break raceLoop // safety: fewer than N clicks arrived
		}
	}

	e.setPhase(PhaseResult, round)

	deltas := map[string]int{}
	for _, ev := range rs.scored {
		deltas[ev.SteamID]++
		scores[ev.SteamID]++
	}
	if len(deltas) > 0 && e.store != nil {
		e.persist(deltas)
	}

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
		RoundID:   newID(),
		Deltas:    deltas,
	})

	e.drain(ctx, e.cfg.ResultDisplay)
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
		case <-e.clicks:
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

func clampPenalty(cur, add, max time.Duration) time.Duration {
	p := cur + add
	if max > 0 && p > max {
		p = max
	}
	return p
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
