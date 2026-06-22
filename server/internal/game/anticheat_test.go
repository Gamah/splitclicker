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

func checksEngine(fastMs, perPlayer int, factor float64) *Engine {
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

// crowd is a multi-player round context with a generous fair-share limit, so the
// fast_clicks / solo_round / dominant tests aren't perturbed by too_many_clicks.
func crowd() checkCtx { return checkCtx{n: 500} }

// fast_clicks fires only when two consecutive scoring clicks are STRICTLY under
// FastClickMs apart (boundary at 130: 129 flags, 130/131 don't).
func TestRunChecksFastClicks(t *testing.T) {
	e := checksEngine(130, 100, 2)

	if got := e.runChecks(scoredAt("a", 0, 129), crowd()); !hasCheck(got, "a", "fast_clicks") {
		t.Fatalf("129ms gap should flag fast_clicks, got %+v", got)
	}
	if got := e.runChecks(scoredAt("a", 0, 130), crowd()); hasCheck(got, "a", "fast_clicks") {
		t.Fatalf("130ms gap should NOT flag fast_clicks, got %+v", got)
	}
	if got := e.runChecks(scoredAt("a", 0, 131), crowd()); hasCheck(got, "a", "fast_clicks") {
		t.Fatalf("131ms gap should NOT flag fast_clicks, got %+v", got)
	}
	// A single scoring click can't form a delta — never flagged.
	if got := e.runChecks(scoredAt("a", 0), crowd()); hasCheck(got, "a", "fast_clicks") {
		t.Fatalf("a lone click should not flag fast_clicks, got %+v", got)
	}
}

// fourScorers returns four other players each scoring once, so a round that also
// has player "a" has 5 distinct scorers → fair share = N / 5.
func fourScorers() []ScoredClick {
	out := scoredAt("b", 50)
	out = append(out, scoredAt("c", 60)...)
	out = append(out, scoredAt("d", 70)...)
	out = append(out, scoredAt("e", 80)...)
	return out
}

// too_many_clicks fires when a player takes MORE than MaxClickFactor × the round's
// fair share, where the share is N / the players who actually SCORED this round.
// With 5 scorers and N=10 → fair=2, factor=2 → limit=4: a taking 5 flags, 4 doesn't.
// Crucially the divisor is who scored, not who was connected — a round only one
// player clicked is never flagged, however many slots they take.
func TestRunChecksTooMany(t *testing.T) {
	e := checksEngine(130, 2, 2)
	ctx := checkCtx{n: 10} // 5 scorers below → fair = 10/5 = 2, limit = 2*2 = 4

	four := append(scoredAt("a", 0, 200, 400, 600), fourScorers()...)
	if got := e.runChecks(four, ctx); hasCheck(got, "a", "too_many_clicks") {
		t.Fatalf("4 clicks (==limit) should NOT flag too_many, got %+v", got)
	}
	five := append(scoredAt("a", 0, 200, 400, 600, 800), fourScorers()...)
	if got := e.runChecks(five, ctx); !hasCheck(got, "a", "too_many_clicks") {
		t.Fatalf("5 clicks (>limit) should flag too_many, got %+v", got)
	}
	// Only ONE player scored (even with a 5-player crowd connected): never flagged,
	// however many slots they take. This is the false positive the divisor fix removes.
	lone := scoredAt("a", 0, 100, 200, 300, 400, 500, 600, 700, 800, 900)
	if got := e.runChecks(lone, ctx); hasCheck(got, "a", "too_many_clicks") {
		t.Fatalf("a round only one player scored should NEVER flag too_many, got %+v", got)
	}
}

// A fractional MaxClickFactor floors to a whole click count: 2.5 × fair(2) = 5,
// so 5 clicks are fine and 6 flag (with 5 distinct scorers, N=10).
func TestRunChecksTooManyFractional(t *testing.T) {
	e := checksEngine(130, 2, 2.5)
	ctx := checkCtx{n: 10} // fair = 10/5 = 2, limit = int(2.5*2) = 5

	five := append(scoredAt("a", 0, 200, 400, 600, 800), fourScorers()...)
	if got := e.runChecks(five, ctx); hasCheck(got, "a", "too_many_clicks") {
		t.Fatalf("5 clicks (==limit) should NOT flag too_many, got %+v", got)
	}
	six := append(scoredAt("a", 0, 200, 400, 600, 800, 1000), fourScorers()...)
	if got := e.runChecks(six, ctx); !hasCheck(got, "a", "too_many_clicks") {
		t.Fatalf("6 clicks (>limit) should flag too_many, got %+v", got)
	}
}

// solo_round fires only when the bounty leader is the LONE entry on the sessions-won
// board (leaderAlone) AND their games-won lead is at least SoloLeadMargin. Connection
// and scorer counts are irrelevant — the board, not the crowd, drives this.
func TestRunChecksSoloRound(t *testing.T) {
	e := New(Config{SoloLeadMargin: 15}, nil, nil, nil)
	lone := func(leader string, margin int) checkCtx {
		return checkCtx{n: 50, leaderID: leader, leadMargin: margin, leaderAlone: true}
	}

	// Alone on the board, the leader, lead ≥ 15 → flag.
	if got := e.runChecks(scoredAt("a", 0, 500, 1000), lone("a", 15)); !hasCheck(got, "a", "solo_round") {
		t.Fatalf("lone leader with a 15 lead should flag solo_round, got %+v", got)
	}
	// Lead below the margin → no flag (newcomer building the board's first wins).
	if got := e.runChecks(scoredAt("a", 0, 500), lone("a", 14)); hasCheck(got, "a", "solo_round") {
		t.Fatalf("lone leader under the margin should NOT flag solo_round, got %+v", got)
	}
	// Someone else leads → no flag.
	if got := e.runChecks(scoredAt("a", 0, 500), lone("b", 99)); hasCheck(got, "a", "solo_round") {
		t.Fatalf("lone non-leader should NOT flag solo_round, got %+v", got)
	}
	// More than one entry on the board (leaderAlone=false) → not a solo round,
	// however large the lead and however few players are connected/scoring.
	if got := e.runChecks(scoredAt("a", 0, 500), checkCtx{n: 50, leaderID: "a", leadMargin: 99}); hasCheck(got, "a", "solo_round") {
		t.Fatalf("a contested board should NOT flag solo_round, got %+v", got)
	}
}

// dominant_winner fires when the top scorer took MORE than 2× a runner-up who
// actually competed (scored ≥ DominantRunnerUpMin). A near-idle runner-up never
// triggers it, so a lone clicker isn't punished.
func TestRunChecksDominantWinner(t *testing.T) {
	e := New(Config{DominantRunnerUpMin: 3}, nil, nil, nil)
	ctx := checkCtx{n: 500} // limit high; isolate dominant

	// a:7, b:3 → runner-up competed (3≥3) and 7 > 2×3 → flag a.
	clicks := append(scoredAt("a", 0, 100, 200, 300, 400, 500, 600), scoredAt("b", 50, 150, 250)...)
	if got := e.runChecks(clicks, ctx); !hasCheck(got, "a", "dominant_winner") {
		t.Fatalf("7 vs 3 should flag dominant_winner for a, got %+v", got)
	}
	// a:7, b:2 → runner-up below the floor (idle) → no flag.
	clicks = append(scoredAt("a", 0, 100, 200, 300, 400, 500, 600), scoredAt("b", 50, 150)...)
	if got := e.runChecks(clicks, ctx); hasCheck(got, "a", "dominant_winner") {
		t.Fatalf("7 vs 2 (idle runner-up) should NOT flag dominant_winner, got %+v", got)
	}
	// a:6, b:3 → exactly 2×, not strictly greater → no flag.
	clicks = append(scoredAt("a", 0, 100, 200, 300, 400, 500), scoredAt("b", 50, 150, 250)...)
	if got := e.runChecks(clicks, ctx); hasCheck(got, "a", "dominant_winner") {
		t.Fatalf("6 vs 3 (==2×) should NOT flag dominant_winner, got %+v", got)
	}
}

// The sanction ladder escalates a repeatedly-flagged player from the math test, to
// a timed cooldown at the threshold, to ignored-for-the-bounty after the grace
// checks — all scoped per bounty.
func TestSanctionLadder(t *testing.T) {
	e := New(Config{CheckCooldownThreshold: 3, CheckCooldownMins: 60, CheckIgnoreAfter: 2}, newCaptureBC(), nil, nil)
	bi := BountyInfo{ID: 1, ResolveAtMs: 9_999_999_999_000}
	ch := CheckResult{SteamID: "x", Type: "fast_clicks", Message: "too fast"}

	// Checks 1–2 (< threshold): test rung — benched behind a math test.
	e.applySanction(ch, bi)
	e.applySanction(ch, bi)
	if !e.underTest["x"] {
		t.Fatal("below the threshold the player should be on the test rung")
	}
	if e.sanctions["x"].Checks != 2 {
		t.Fatalf("checks = %d, want 2", e.sanctions["x"].Checks)
	}

	// Check 3 (== threshold): cooldown starts, the test rung is cleared.
	e.applySanction(ch, bi)
	if e.underTest["x"] {
		t.Fatal("at the threshold the player should leave the test rung")
	}
	if e.sanctions["x"].CooldownUntil == nil {
		t.Fatal("at the threshold a cooldown should start")
	}
	if e.sanctions["x"].Ignored {
		t.Fatal("at the threshold the player should not yet be ignored")
	}

	// Checks 4–5 (threshold + grace): ignored for the rest of the bounty.
	e.applySanction(ch, bi)
	e.applySanction(ch, bi)
	if !e.sanctions["x"].Ignored {
		t.Fatal("past the grace checks the player should be ignored")
	}
	if !e.blockedMap()["x"] {
		t.Fatal("an ignored player must be blocked from scoring")
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
