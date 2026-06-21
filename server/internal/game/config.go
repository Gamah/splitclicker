package game

import (
	"os"
	"strconv"
	"time"
)

// Config holds the game tunables. All are server-side; see ConfigFromEnv.
type Config struct {
	ArmMin          time.Duration // shortest arming delay
	ArmMax          time.Duration // longest arming delay
	ClicksPerPlayer int           // N = ClicksPerPlayer × connected players (the scoring slots scale with the crowd)
	MinClicks       int           // floor for N when few/no players are connected (1 = first-click-wins)
	RoundsPerGame   int           // rounds before game_over
	ButtonsOnScreen int           // X: live buttons shown at once during an armed window (v5+). The
	//                              board is refilled to this count after every claim; the round still
	//                              ends when N total points are claimed, so up to X−1 unclaimed buttons
	//                              can be on screen at the final claim. 1 ⇒ single-button (legacy-shaped).
	RaceMax       time.Duration // safety cap: close the race even if < N clicks land
	ResultDisplay time.Duration // how long the round leaderboard shows
	Intermission  time.Duration // pause between games

	BoardSize int // top-K standings included in result/game_over frames

	// Live-round tick (the coalesced armed-window broadcast; see Engine.race). While
	// the button is armed the engine emits a `tick` frame TickHz times a second
	// carrying the running clicks-remaining count, every board mutation since the last
	// tick (buttons claimed + their server-RNG'd replacements — these are authoritative
	// and never sampled, or a client would miss a live button), and a bounded SAMPLE of
	// connected players' cursor positions. The fan-out is linear in players (one
	// precomputed broadcast per tick) and the board-mutation bytes are O(scoring clicks),
	// never per-non-scoring-click. TickHz<=0 disables ticking entirely.
	//   TickHz       — ticks per second while armed (0 = off).
	//   TickSampleK  — max opponent cursors sampled per tick (cosmetic, so capped). The
	//                  clicks-remaining count and the claim/spawn mutations are always
	//                  exact; only the cursor sample is bounded.
	TickHz      int
	TickSampleK int

	// Bad-click penalty escalation (ms): the kth bad click since the last arm adds
	// PenaltyBaseMs + PenaltyStepMs·(k−1) to that connection's held arm delay. Sent
	// to clients on connect so they mirror the live estimate. See idlePenalty.
	PenaltyBaseMs int
	PenaltyStepMs int

	// Anticheat checks (run at the end of every round; see Engine.runChecks).
	//   FastClickMs         — flags a player whose two consecutive SCORING clicks
	//                         landed less than this many ms apart (autoclicker-fast).
	//   MaxClickFactor      — flags a player who took more than MaxClickFactor × the
	//                         round's fair share (N / active players) of the scoring
	//                         slots. Skipped in solo rounds (one player legitimately
	//                         takes every slot).
	//   SoloLeadMargin      — solo_round only fires once a lone leader's games-won
	//                         lead over second place is at least this many wins, so a
	//                         newcomer alone on the server isn't punished.
	//   DominantRunnerUpMin — dominant_winner only fires when the runner-up actually
	//                         competed (scored at least this many clicks); guards the
	//                         "one player clicks, the other is idle" false positive.
	FastClickMs         int
	MaxClickFactor      int
	SoloLeadMargin      int
	DominantRunnerUpMin int

	// Anticheat sanction ladder (per bounty, per player; see Engine.applySanction).
	// The first CheckCooldownThreshold-1 checks each bench the player behind a math
	// test. The CheckCooldownThreshold'th check starts a CheckCooldownMins cooldown
	// (clicks ignored, no test). CheckIgnoreAfter more checks past that sidelines
	// them until the bounty resolves. Counts reset when the bounty changes.
	CheckCooldownThreshold int
	CheckCooldownMins      int
	CheckIgnoreAfter       int
}

// DefaultConfig is the baseline tuning (overridable via data/config.json, then env).
func DefaultConfig() Config {
	return Config{
		ArmMin:          2 * time.Second,
		ArmMax:          6 * time.Second,
		ClicksPerPlayer: 15,
		MinClicks:       50,
		RoundsPerGame:   5,
		ButtonsOnScreen: 10,
		RaceMax:         5 * time.Second,
		ResultDisplay:   4 * time.Second,
		Intermission:    5 * time.Second,
		BoardSize:       20,
		TickHz:          20,
		TickSampleK:     8,
		PenaltyBaseMs:   500,
		PenaltyStepMs:   100,
		FastClickMs:         130,
		MaxClickFactor:      2,
		SoloLeadMargin:      15,
		DominantRunnerUpMin: 5,

		CheckCooldownThreshold: 20,
		CheckCooldownMins:      60,
		CheckIgnoreAfter:       2,
	}
}

// ConfigFromEnv starts from DefaultConfig and overrides any tunable whose env
// var is set. Durations ending in Sec/Ms are read as integers in that unit.
func ConfigFromEnv() Config {
	c := DefaultConfig()
	c.ArmMin = envDur("ARM_MIN_SEC", c.ArmMin, time.Second)
	c.ArmMax = envDur("ARM_MAX_SEC", c.ArmMax, time.Second)
	c.ClicksPerPlayer = envInt("CLICKS_PER_PLAYER", c.ClicksPerPlayer)
	c.MinClicks = envInt("MIN_CLICKS", c.MinClicks)
	c.RoundsPerGame = envInt("ROUNDS_PER_GAME", c.RoundsPerGame)
	c.ButtonsOnScreen = envInt("BUTTONS_ON_SCREEN", c.ButtonsOnScreen)
	c.RaceMax = envDur("RACE_MAX_MS", c.RaceMax, time.Millisecond)
	c.ResultDisplay = envDur("RESULT_DISPLAY_MS", c.ResultDisplay, time.Millisecond)
	c.Intermission = envDur("INTERMISSION_MS", c.Intermission, time.Millisecond)
	c.BoardSize = envInt("BOARD_SIZE", c.BoardSize)
	c.TickHz = envInt("TICK_HZ", c.TickHz)
	c.TickSampleK = envInt("TICK_SAMPLE_K", c.TickSampleK)
	c.PenaltyBaseMs = envInt("PENALTY_BASE_MS", c.PenaltyBaseMs)
	c.PenaltyStepMs = envInt("PENALTY_STEP_MS", c.PenaltyStepMs)
	c.FastClickMs = envInt("FAST_CLICK_MS", c.FastClickMs)
	c.MaxClickFactor = envInt("MAX_CLICK_FACTOR", c.MaxClickFactor)
	c.SoloLeadMargin = envInt("SOLO_LEAD_MARGIN", c.SoloLeadMargin)
	c.DominantRunnerUpMin = envInt("DOMINANT_RUNNER_UP_MIN", c.DominantRunnerUpMin)
	c.CheckCooldownThreshold = envInt("CHECK_COOLDOWN_THRESHOLD", c.CheckCooldownThreshold)
	c.CheckCooldownMins = envInt("CHECK_COOLDOWN_MINS", c.CheckCooldownMins)
	c.CheckIgnoreAfter = envInt("CHECK_IGNORE_AFTER", c.CheckIgnoreAfter)
	return c
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDur(key string, def time.Duration, unit time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * unit
		}
	}
	return def
}
