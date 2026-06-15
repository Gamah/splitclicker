package game

import (
	"context"
	"sync"
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
	// The Nth idle click adds N×5ms, so totals run 5,15,30,50,75,105… ms.
	want := []time.Duration{0, 5, 15, 30, 50, 75, 105}
	for n, w := range want {
		if got := idlePenalty(n); got != w*time.Millisecond {
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
func (b *captureBC) PlayerCount() int         { return 1 }

// TestEngineLoopScores runs the real timed loop with tiny delays: one round,
// N=1, fire a valid click on arm, assert it scores and the game ends.
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

	select {
	case r := <-bc.result:
		if r.Deltas["winner"] != 1 {
			t.Fatalf("winner should have delta 1, got %v", r.Deltas)
		}
		if len(r.Winners) != 1 || r.Winners[0].SteamID != "winner" {
			t.Fatalf("unexpected winners: %+v", r.Winners)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no result frame")
	}

	select {
	case g := <-bc.gameOver:
		if g.Placements["winner"] != 1 || !g.Won["winner"] {
			t.Fatalf("winner should place 1 and win: %+v %+v", g.Placements, g.Won)
		}
	case <-time.After(time.Second):
		t.Fatal("no game_over frame")
	}
}
