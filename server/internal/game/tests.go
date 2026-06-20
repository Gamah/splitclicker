package game

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"go.uber.org/zap"
)

// pendingTest is one outstanding anticheat test for a benched player. id is the
// token a correct answer must echo; expected is the answer string to match.
type pendingTest struct {
	id       string
	kind     string
	prompt   string
	expected string
}

// sum2Pool is the full bank of "add two DISTINCT 2-digit numbers" problems:
// every ordered pair (a, b) with a, b in [10, 99] and a != b. That is
// 90 × 89 = 8010 unique tests. Built once at package load.
var sum2Pool = buildSum2Pool()

func buildSum2Pool() [][2]int {
	pool := make([][2]int, 0, 8010)
	for a := 10; a <= 99; a++ {
		for b := 10; b <= 99; b++ {
			if a == b {
				continue // distinct numbers only (avoids trivial "double it" prompts)
			}
			pool = append(pool, [2]int{a, b})
		}
	}
	return pool
}

// newTest builds a fresh test for a benched player (currently always a sum2 drawn
// uniformly from the 8010-problem pool), records it for the audit trail, and
// returns it. The store write is fire-and-forget so it never blocks the Run loop.
func (e *Engine) newTest(steamID string) pendingTest {
	e.rngMu.Lock()
	p := sum2Pool[e.rng.Intn(len(sum2Pool))]
	e.rngMu.Unlock()

	a, b := p[0], p[1]
	pt := pendingTest{
		id:       newID(),
		kind:     "sum2",
		prompt:   fmt.Sprintf("%d + %d", a, b),
		expected: strconv.Itoa(a + b),
	}
	if e.store != nil {
		rec := TestRecord{ID: pt.id, SteamID: steamID, Kind: pt.kind, Prompt: pt.prompt, Expected: pt.expected}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := e.store.RecordTestSent(ctx, rec); err != nil {
				e.log.Error("persist anticheat test", zap.Error(err))
			}
		}()
	}
	return pt
}

// recordTestAnswer persists a settled test answer off the Run loop.
func (e *Engine) recordTestAnswer(id, answer string, correct bool) {
	if e.store == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := e.store.RecordTestAnswer(ctx, id, answer, correct); err != nil {
			e.log.Error("persist anticheat test answer", zap.Error(err))
		}
	}()
}
