package game

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func click(steamID string, nonce uint64) ClickEvent {
	return ClickEvent{SteamID: steamID, Tag: steamID, Username: steamID, Nonce: nonce, At: time.Now()}
}

// raceState is the whole scoring rule; test it directly (no timing).
func TestRaceState(t *testing.T) {
	const nonce = uint64(0xABCD)

	t.Run("first N by arrival score, rest dropped", func(t *testing.T) {
		rs := newRaceState(nonce, 2)
		if !rs.offer(click("a", nonce)) {
			t.Fatal("a should score")
		}
		if !rs.offer(click("b", nonce)) {
			t.Fatal("b should score")
		}
		if !rs.full() {
			t.Fatal("race should be full at N=2")
		}
		if rs.offer(click("c", nonce)) {
			t.Fatal("c arrived after N — must not score")
		}
		if len(rs.scored) != 2 || rs.scored[0].SteamID != "a" || rs.scored[1].SteamID != "b" {
			t.Fatalf("wrong winners/order: %+v", rs.scored)
		}
	})

	t.Run("wrong or zero nonce never scores (anti-pre-fire)", func(t *testing.T) {
		rs := newRaceState(nonce, 1)
		if rs.offer(click("a", nonce+1)) {
			t.Fatal("wrong nonce must not score")
		}
		if rs.offer(click("a", 0)) {
			t.Fatal("zero nonce must not score")
		}
		if rs.full() {
			t.Fatal("nothing valid scored yet")
		}
		if !rs.offer(click("a", nonce)) {
			t.Fatal("correct nonce should score")
		}
	})

	t.Run("a single player can take multiple slots in one arm", func(t *testing.T) {
		rs := newRaceState(nonce, 3)
		if !rs.offer(click("a", nonce)) {
			t.Fatal("a first click should score")
		}
		if !rs.offer(click("a", nonce)) {
			t.Fatal("a second click in same arm should also score")
		}
		if !rs.offer(click("a", nonce)) {
			t.Fatal("a third click should fill the race")
		}
		if !rs.full() {
			t.Fatal("race should be full at N=3")
		}
		if rs.offer(click("a", nonce)) {
			t.Fatal("a fourth click after N must not score")
		}
		if len(rs.scored) != 3 {
			t.Fatalf("expected 3 scores, got %d", len(rs.scored))
		}
	})
}

func TestIdlePenalty(t *testing.T) {
	// The kth bad click since the last arm adds 500+100·(k−1) ms, so totals run
	// 0,500,1100,1800,2600,3500,4500… ms.
	e := New(Config{PenaltyBaseMs: 500, PenaltyStepMs: 100}, nil, nil, nil)
	want := []time.Duration{0, 500, 1100, 1800, 2600, 3500, 4500}
	for n, w := range want {
		if got := e.idlePenalty(n); got != w*time.Millisecond {
			t.Fatalf("idlePenalty(%d) = %v, want %v", n, got, w*time.Millisecond)
		}
	}
}

func TestStandingsOrder(t *testing.T) {
	scores := map[string]int{"a": 1, "b": 3, "c": 3}
	info := map[string]playerInfo{"a": {}, "b": {}, "c": {}}
	s := standingsOf(scores, info)
	// b,c tie at 3 — SteamID asc tiebreak puts b before c; a last.
	if s[0].SteamID != "b" || s[1].SteamID != "c" || s[2].SteamID != "a" {
		t.Fatalf("unexpected order: %v %v %v", s[0].SteamID, s[1].SteamID, s[2].SteamID)
	}
}

func TestRandArmDelayWithinBounds(t *testing.T) {
	e := New(Config{ArmMin: 10 * time.Millisecond, ArmMax: 50 * time.Millisecond}, nil, nil, nil)
	for i := 0; i < 1000; i++ {
		d := e.randArmDelay()
		if d < 10*time.Millisecond || d > 50*time.Millisecond {
			t.Fatalf("arm delay %v out of [10ms,50ms]", d)
		}
	}
}

// captureBC records broadcast frames and exposes the armed nonce so a test can
// form a valid click.
type captureBC struct {
	mu       sync.Mutex
	armed    chan ArmedFrame
	result   chan ResultFrame
	gameOver chan GameOverFrame
}

func newCaptureBC() *captureBC {
	return &captureBC{
		armed:    make(chan ArmedFrame, 8),
		result:   make(chan ResultFrame, 8),
		gameOver: make(chan GameOverFrame, 8),
	}
}

func (b *captureBC) Pending(PendingFrame)     {}
func (b *captureBC) Armed(a ArmedFrame)       { b.armed <- a }
func (b *captureBC) Result(r ResultFrame)     { b.result <- r }
func (b *captureBC) GameOver(g GameOverFrame) { b.gameOver <- g }
func (b *captureBC) DevNote(string)           {}
func (b *captureBC) PlayerCount() int         { return 1 }
func (b *captureBC) ActivePlayerCount(map[string]bool) int { return 1 }
func (b *captureBC) SendTest(string, TestFrame)            {}
func (b *captureBC) TestCapable(string) bool               { return true }

// TestEngineLoopScores runs the real timed loop with tiny delays: one round
// (which is therefore the final round), N=1, fire a valid click on arm, assert it
// scores and the game folds straight into game_over — no separate round_result.
func TestEngineLoopScores(t *testing.T) {
	cfg := Config{
		ArmMin: 10 * time.Millisecond, ArmMax: 10 * time.Millisecond,
		MinClicks: 1, RoundsPerGame: 1,
		RaceMax: 2 * time.Second, ResultDisplay: 5 * time.Millisecond,
		Intermission: 5 * time.Millisecond, BoardSize: 20,
	}
	bc := newCaptureBC()
	e := New(cfg, bc, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)

	var armed ArmedFrame
	select {
	case armed = <-bc.armed:
	case <-time.After(time.Second):
		t.Fatal("no armed frame")
	}

	e.Submit(click("winner", armed.Nonce))

	// The only round is the final round, so it must NOT emit a round_result.
	select {
	case g := <-bc.gameOver:
		if g.Placements["winner"] != 1 || !g.Won["winner"] {
			t.Fatalf("winner should place 1 and win: %+v %+v", g.Placements, g.Won)
		}
		if g.Deltas["winner"] != 1 {
			t.Fatalf("game_over should carry the final round's delta 1, got %v", g.Deltas)
		}
		if g.RoundID == "" {
			t.Fatal("game_over should carry the final round's round id")
		}
	case <-bc.result:
		t.Fatal("final round must fold into game_over, not emit round_result")
	case <-time.After(3 * time.Second):
		t.Fatal("no game_over frame")
	}
}

// pausableBC is a captureBC whose player count is settable, to drive the
// engine's pause-when-empty behaviour.
type pausableBC struct {
	*captureBC
	players atomic.Int32
}

func (b *pausableBC) PlayerCount() int { return int(b.players.Load()) }

// TestEnginePausesWithoutPlayers: with no players connected the engine arms
// nothing and writes no history; once a client connects (and wakes it) a game
// starts promptly.
func TestEnginePausesWithoutPlayers(t *testing.T) {
	cfg := Config{
		ArmMin: 10 * time.Millisecond, ArmMax: 10 * time.Millisecond,
		MinClicks: 1, RoundsPerGame: 1,
		RaceMax: 2 * time.Second, ResultDisplay: 5 * time.Millisecond,
		Intermission: 5 * time.Millisecond, BoardSize: 20,
	}
	bc := &pausableBC{captureBC: newCaptureBC()} // players starts at 0
	st := &fakeStore{}
	e := New(cfg, bc, st, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)

	// Paused: no round arms while nobody is connected.
	select {
	case <-bc.armed:
		t.Fatal("engine armed a round with no players connected")
	case <-time.After(200 * time.Millisecond):
	}
	st.mu.Lock()
	n := len(st.logs)
	st.mu.Unlock()
	if n != 0 {
		t.Fatalf("expected no games recorded while paused, got %d", n)
	}

	// A client connects and wakes the engine — a game should start at once.
	bc.players.Store(1)
	e.Wake()
	select {
	case <-bc.armed:
	case <-time.After(time.Second):
		t.Fatal("engine did not start a game after a player connected")
	}
}

// fakeStore captures the GameLog handed to RecordGame so a test can assert the
// per-click history a completed game produces. Implements game.Store.
type fakeStore struct {
	mu   sync.Mutex
	logs []GameLog
}

func (s *fakeStore) AddHourlyPoints(context.Context, time.Time, []HourlyDelta) error { return nil }
func (s *fakeStore) AddSessionWin(context.Context, string) error                     { return nil }
func (s *fakeStore) RecordGame(_ context.Context, log GameLog) error {
	s.mu.Lock()
	s.logs = append(s.logs, log)
	s.mu.Unlock()
	return nil
}
func (s *fakeStore) RecordTestSent(context.Context, TestRecord) error          { return nil }
func (s *fakeStore) RecordTestAnswer(context.Context, string, string, bool) error { return nil }

// TestEngineRecordsGameHistory: a 2-round game (N=2) records exactly one GameLog
// whose rounds carry the scoring clicks in arrival order — contiguous SlotNo
// 0..n-1, the expected SteamID per slot, and non-negative OffsetMs.
func TestEngineRecordsGameHistory(t *testing.T) {
	cfg := Config{
		ArmMin: 10 * time.Millisecond, ArmMax: 10 * time.Millisecond,
		MinClicks: 2, RoundsPerGame: 2,
		RaceMax: 2 * time.Second, ResultDisplay: 5 * time.Millisecond,
		Intermission: 5 * time.Millisecond, BoardSize: 20,
	}
	bc := newCaptureBC()
	st := &fakeStore{}
	e := New(cfg, bc, st, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)

	// Fill both rounds' N=2 slots: "a" then "b" each round.
	a1 := <-bc.armed
	e.Submit(click("a", a1.Nonce))
	e.Submit(click("b", a1.Nonce))
	<-bc.result // round 1 done

	a2 := <-bc.armed
	e.Submit(click("a", a2.Nonce))
	e.Submit(click("b", a2.Nonce))
	<-bc.gameOver // final round folds into game_over; afterGame runs next

	// afterGame is a detached goroutine — wait briefly for the RecordGame write.
	var log GameLog
	deadline := time.After(2 * time.Second)
	for {
		st.mu.Lock()
		n := len(st.logs)
		if n > 0 {
			log = st.logs[0]
		}
		st.mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no GameLog recorded")
		case <-time.After(5 * time.Millisecond):
		}
	}

	if log.GameID == "" || log.StartedAt.IsZero() || log.EndedAt.IsZero() {
		t.Fatalf("game metadata not populated: %+v", log)
	}
	if len(log.RoundLogs) != 2 {
		t.Fatalf("expected 2 rounds logged, got %d", len(log.RoundLogs))
	}
	for i, r := range log.RoundLogs {
		if r.RoundNo != i+1 {
			t.Fatalf("round %d has RoundNo %d", i, r.RoundNo)
		}
		if len(r.Clicks) != 2 {
			t.Fatalf("round %d expected 2 scoring clicks, got %d", r.RoundNo, len(r.Clicks))
		}
		wantSID := []string{"a", "b"}
		for slot, c := range r.Clicks {
			if c.SlotNo != slot {
				t.Fatalf("round %d slot %d has SlotNo %d", r.RoundNo, slot, c.SlotNo)
			}
			if c.SteamID != wantSID[slot] {
				t.Fatalf("round %d slot %d: want %s, got %s", r.RoundNo, slot, wantSID[slot], c.SteamID)
			}
			if c.OffsetMs < 0 {
				t.Fatalf("round %d slot %d: negative OffsetMs %d", r.RoundNo, slot, c.OffsetMs)
			}
		}
	}
}

// TestEngineFinalRoundFoldsIntoGameOver: a 2-round game emits exactly one
// round_result (round 1), then game_over for the final round.
func TestEngineFinalRoundFoldsIntoGameOver(t *testing.T) {
	cfg := Config{
		ArmMin: 10 * time.Millisecond, ArmMax: 10 * time.Millisecond,
		MinClicks: 1, RoundsPerGame: 2,
		RaceMax: 2 * time.Second, ResultDisplay: 5 * time.Millisecond,
		Intermission: 5 * time.Millisecond, BoardSize: 20,
	}
	bc := newCaptureBC()
	e := New(cfg, bc, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)

	// Round 1 (non-final): scores, emits a round_result.
	a1 := <-bc.armed
	e.Submit(click("winner", a1.Nonce))
	select {
	case r := <-bc.result:
		if r.Round != 1 {
			t.Fatalf("first result should be round 1, got %d", r.Round)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no round_result for round 1")
	}

	// Round 2 (final): scores, folds into game_over with no second round_result.
	a2 := <-bc.armed
	e.Submit(click("winner", a2.Nonce))
	select {
	case g := <-bc.gameOver:
		if g.Deltas["winner"] != 1 {
			t.Fatalf("game_over should carry the final round's delta 1, got %v", g.Deltas)
		}
	case <-bc.result:
		t.Fatal("final round must not emit a second round_result")
	case <-time.After(3 * time.Second):
		t.Fatal("no game_over frame")
	}
}
