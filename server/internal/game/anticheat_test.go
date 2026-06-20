package game

import "testing"

// The sum2 bank must be exactly the 90×89 = 8010 ordered pairs of DISTINCT
// 2-digit numbers, with no duplicates and never a == b.
func TestSum2PoolSize(t *testing.T) {
	if len(sum2Pool) != 8010 {
		t.Fatalf("sum2Pool size = %d, want 8010", len(sum2Pool))
	}
	seen := make(map[[2]int]bool, len(sum2Pool))
	for _, p := range sum2Pool {
		a, b := p[0], p[1]
		if a < 10 || a > 99 || b < 10 || b > 99 {
			t.Fatalf("pair out of 2-digit range: %v", p)
		}
		if a == b {
			t.Fatalf("pair has equal numbers: %v", p)
		}
		if seen[p] {
			t.Fatalf("duplicate pair: %v", p)
		}
		seen[p] = true
	}
}

func checksEngine(fastMs, perPlayer, factor int) *Engine {
	return New(Config{FastClickMs: fastMs, ClicksPerPlayer: perPlayer, MaxClickFactor: factor}, nil, nil, nil)
}

func scoredAt(steamID string, offsets ...int) []ScoredClick {
	out := make([]ScoredClick, len(offsets))
	for i, off := range offsets {
		out[i] = ScoredClick{SteamID: steamID, SlotNo: i, OffsetMs: off}
	}
	return out
}

func hasCheck(checks []CheckResult, sid, typ string) bool {
	for _, c := range checks {
		if c.SteamID == sid && c.Type == typ {
			return true
		}
	}
	return false
}

// fast_clicks fires only when two consecutive scoring clicks are STRICTLY under
// FastClickMs apart (boundary at 130: 129 flags, 130/131 don't).
func TestRunChecksFastClicks(t *testing.T) {
	e := checksEngine(130, 100, 2) // high per-player limit so too_many never trips

	if got := e.runChecks(scoredAt("a", 0, 129)); !hasCheck(got, "a", "fast_clicks") {
		t.Fatalf("129ms gap should flag fast_clicks, got %+v", got)
	}
	if got := e.runChecks(scoredAt("a", 0, 130)); hasCheck(got, "a", "fast_clicks") {
		t.Fatalf("130ms gap should NOT flag fast_clicks, got %+v", got)
	}
	if got := e.runChecks(scoredAt("a", 0, 131)); hasCheck(got, "a", "fast_clicks") {
		t.Fatalf("131ms gap should NOT flag fast_clicks, got %+v", got)
	}
	// A single scoring click can't form a delta — never flagged.
	if got := e.runChecks(scoredAt("a", 0)); hasCheck(got, "a", "fast_clicks") {
		t.Fatalf("a lone click should not flag fast_clicks, got %+v", got)
	}
}

// too_many_clicks fires when a player takes MORE than MaxClickFactor×ClicksPerPlayer
// scoring slots (limit 4 here: 5 flags, 4 doesn't). Offsets are spaced wide so the
// fast_clicks rule never interferes.
func TestRunChecksTooMany(t *testing.T) {
	e := checksEngine(130, 2, 2) // limit = 2*2 = 4

	four := scoredAt("a", 0, 200, 400, 600)
	if got := e.runChecks(four); hasCheck(got, "a", "too_many_clicks") {
		t.Fatalf("4 clicks (==limit) should NOT flag too_many, got %+v", got)
	}
	five := scoredAt("a", 0, 200, 400, 600, 800)
	if got := e.runChecks(five); !hasCheck(got, "a", "too_many_clicks") {
		t.Fatalf("5 clicks (>limit) should flag too_many, got %+v", got)
	}
}

// A correct answer clears the bench; a wrong one keeps the player benched and
// re-issues a fresh test (new id).
func TestHandleAnswer(t *testing.T) {
	e := New(Config{}, newCaptureBC(), nil, nil)
	e.underTest["x"] = true
	e.pendingTests["x"] = pendingTest{id: "t1", kind: "sum2", prompt: "10 + 20", expected: "30"}

	// Wrong answer: still benched, and the outstanding test is replaced.
	e.handleAnswer(answerEvent{SteamID: "x", ID: "t1", Answer: "99"})
	if !e.underTest["x"] {
		t.Fatal("wrong answer should keep the player benched")
	}
	npt, ok := e.pendingTests["x"]
	if !ok || npt.id == "t1" {
		t.Fatalf("wrong answer should issue a fresh test, got %+v (ok=%v)", npt, ok)
	}

	// A stale answer (wrong id) is ignored.
	e.handleAnswer(answerEvent{SteamID: "x", ID: "t1", Answer: npt.expected})
	if !e.underTest["x"] {
		t.Fatal("answer with a stale id should be ignored")
	}

	// Correct answer to the current test clears the bench.
	e.handleAnswer(answerEvent{SteamID: "x", ID: npt.id, Answer: npt.expected})
	if e.underTest["x"] {
		t.Fatal("correct answer should un-bench the player")
	}
	if _, ok := e.pendingTests["x"]; ok {
		t.Fatal("correct answer should drop the pending test")
	}
}
