package game

import "time"

// --- multi-button board (v5) ---
//
// A round's armed window shows up to Config.ButtonsOnScreen ("X") live buttons at
// once. Each button is a one-shot scoring target: the first valid click that echoes
// its secret nonce claims it for +1 point, the button is consumed, and — as long as
// the round still has budget left — a replacement spawns at a fresh server-RNG'd
// position with a fresh nonce, keeping the board refilled to X. The round still ends
// the instant N total points are claimed (board.full), so up to X−1 unclaimed buttons
// can be on screen at the final claim; they're simply discarded.
//
// Positions are server-authoritative and transmitted (initial X in the armed frame,
// each replacement in its tick claim event) — never derived client-side from a seed,
// so a client cannot pre-compute where buttons will appear and pre-aim. See PLAN §8.

// Button is one live scoring target during an armed window. SlotID is the compact
// wire handle the armed/tick frames reference (cheap to ship); Nonce is the secret a
// scoring click must echo (anti-pre-fire — unguessable until the button appears);
// X,Y is its server-RNG'd normalized position (int16, 0 = centre, matching the click
// coordinate convention). Consumed by the first valid click echoing Nonce.
type Button struct {
	SlotID uint16
	Nonce  uint64
	X, Y   int16
}

// BoardClaim is one board mutation carried in a tick frame: the button at SlotID was
// claimed by ClaimerTag at TArmMs (ms since arm, so the client jitter-buffer replays
// the pip at its true moment). Spawn is the server-RNG'd replacement, or nil when this
// claim ended the round (no refill). The client removes SlotID — drawing the opponent
// pip at that button's known position, labelled with ClaimerTag's username — then adds
// Spawn. Claims are authoritative and never sampled (a dropped claim would leave a
// client showing a dead button or missing a live one).
type BoardClaim struct {
	SlotID     uint16
	ClaimerTag string
	TArmMs     uint16
	Spawn      *Button
}

// board is the multi-button scoring state for one armed window. It generalizes the
// old single-nonce raceState: scored is the clicks that took a slot in arrival order
// (identical to before — used for the round's deltas/history/anticheat), pending is
// the board mutations accumulated since the last tick (drained by the tick emitter).
// Touched only from the engine's Run goroutine, so it needs no lock.
type board struct {
	n        int               // scoring budget for the round (== old raceState.n)
	live     map[uint16]Button // slotID → live button
	byNonce  map[uint64]uint16 // live nonce → slotID (scoring lookup)
	nextSlot uint16            // monotonic slot-id counter
	armedAt  time.Time
	scored   []ClickEvent // arrival order (drives deltas/scores/reached/history)
	pending  []BoardClaim // mutations since the last tick

	// mint spawns a fresh button (new nonce + server-RNG'd, non-overlapping position)
	// and registers it as live. Supplied by the engine so the board stays free of the
	// rng/nonce dependencies; see Engine.race.
	mint func() Button
}

func newBoard(n int, armedAt time.Time) *board {
	if n < 1 {
		n = 1
	}
	return &board{
		n:       n,
		live:    map[uint16]Button{},
		byNonce: map[uint64]uint16{},
		armedAt: armedAt,
	}
}

// full reports whether the round's N budget is spent (every scored click took one
// button slot), which closes the window — exactly as before.
func (b *board) full() bool { return len(b.scored) >= b.n }

// register assigns the next slot id to a (nonce, position) and marks it live.
func (b *board) register(nonce uint64, x, y int16) Button {
	b.nextSlot++
	btn := Button{SlotID: b.nextSlot, Nonce: nonce, X: x, Y: y}
	b.live[btn.SlotID] = btn
	b.byNonce[nonce] = btn.SlotID
	return btn
}

// positions returns the live buttons, for the placement RNG's overlap check.
func (b *board) positions() []Button {
	out := make([]Button, 0, len(b.live))
	for _, btn := range b.live {
		out = append(out, btn)
	}
	return out
}

// offer reports whether ev scored, applying the whole scoring rule. A pre-fire/garbage
// click (nonce 0) or a spent budget scores nothing; a click echoing a live button's
// nonce claims it (consuming it, recording a mutation, and refilling the board unless
// the round just ended). A click echoing a consumed or unknown non-zero nonce scores
// nothing and — like before — is not penalised (it's a legitimate lost race; only
// nonce 0 penalises, via Engine.recordBad).
func (b *board) offer(ev ClickEvent) bool {
	if b.full() || ev.Nonce == 0 {
		return false
	}
	slot, ok := b.byNonce[ev.Nonce]
	if !ok {
		return false // consumed (lost race) or wrong nonce
	}
	delete(b.live, slot)
	delete(b.byNonce, ev.Nonce)
	b.scored = append(b.scored, ev)
	claim := BoardClaim{SlotID: slot, ClaimerTag: ev.Tag, TArmMs: msSince(b.armedAt, ev.At)}
	if !b.full() { // budget remains ⇒ keep the board topped up to X
		nb := b.mint()
		claim.Spawn = &nb
	}
	b.pending = append(b.pending, claim)
	return true
}

// takePending returns the board mutations since the last call and clears them, so each
// tick carries only the new ones.
func (b *board) takePending() []BoardClaim {
	if len(b.pending) == 0 {
		return nil
	}
	out := b.pending
	b.pending = nil
	return out
}

// msSince is the millisecond offset of t after armedAt, clamped to the uint16 the wire
// carries (the jitter-buffer replay offset; same clamp the pips used).
func msSince(armedAt, t time.Time) uint16 {
	ms := t.Sub(armedAt).Milliseconds()
	if ms < 0 {
		return 0
	}
	if ms > 65535 {
		return 65535
	}
	return uint16(ms)
}

// Button-placement RNG bounds (normalized int16 coordinates, 0 = centre). Positions are
// kept inside ±posBound so a button's body stays on-screen, and at least posMinDist apart
// (best-effort, posMaxTries reject samples) so the initial X don't overlap. These are
// server-side heuristics — the client must size buttons to roughly match; tune freely.
const (
	posBound    = 29000
	posMinDist  = 6000
	posMaxTries = 12
)

// randPos picks a normalized button position avoiding the existing live buttons. It
// reject-samples up to posMaxTries for a spot at least posMinDist from every existing
// button, then gives up and returns the last candidate (a rare overlap beats looping).
// Uses the engine rng under rngMu — called only from the Run goroutine, but the lock
// keeps it safe alongside randArmDelay.
func (e *Engine) randPos(existing []Button) (int16, int16) {
	const span = 2*posBound + 1
	e.rngMu.Lock()
	defer e.rngMu.Unlock()
	var x, y int16
	for try := 0; try < posMaxTries; try++ {
		x = int16(e.rng.Intn(span) - posBound)
		y = int16(e.rng.Intn(span) - posBound)
		ok := true
		for _, btn := range existing {
			if abs32(int32(x)-int32(btn.X)) < posMinDist && abs32(int32(y)-int32(btn.Y)) < posMinDist {
				ok = false
				break
			}
		}
		if ok {
			return x, y
		}
	}
	return x, y
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}
