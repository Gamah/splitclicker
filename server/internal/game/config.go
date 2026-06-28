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

	// Anticheat checks (run at the end of every round; see Engine.runChecks).
	//   FastClickMs         — flags a player whose two consecutive SCORING clicks
	//                         landed less than this many ms apart (autoclicker-fast).
	//   MaxClickFactor      — flags a player who took more than MaxClickFactor × the
	//                         round's fair share (N / active players) of the scoring
	//                         slots. Fractional (e.g. 2.5) is allowed; the limit is
	//                         floored to a whole click count. Skipped in solo rounds
	//                         (one player legitimately takes every slot).
	//   SoloLeadMargin      - solo_round (the session-level check; see checkSoloSession)
	//                         only fires once the bounty leader's lead AFTER winning an
	//                         uncontested session strictly exceeds this: the gap over the
	//                         runner-up, or their own games-won total when alone on the
	//                         board. So with the default 4 it first fires at a lead of 5,
	//                         leaving a newcomer building the board's first wins room to play.
	//   DominantRunnerUpMin — dominant_winner only fires when the runner-up actually
	//                         competed (scored at least this many clicks); guards the
	//                         "one player clicks, the other is idle" false positive.
	//   AfkCheck            - enable gate for the AFK pass (>0 on, 0 off; see checkAfk).
	//                         AFK is now a purely MOVEMENT signal evaluated at the END of the
	//                         arming phase (between round_pending and armed): a player who
	//                         sent no cursor frame during arming, OR whose cursor never moved,
	//                         is AFK and is parked BEFORE the button arms, so they never take a
	//                         scoring slot. Movement is BINARY (any change of position counts),
	//                         so there is no threshold to tune. Score NEVER enters AFK logic
	//                         (the wire-bot signature is caught by the separate `busted` check).
	//                         Only v7+ clients send arming cursors, so only they are AFK-checked
	//                         (v6 sends cursors armed-only and is exempt — see minArmingVersion).
	FastClickMs         int
	MaxClickFactor      float64
	SoloLeadMargin      int
	DominantRunnerUpMin int
	AfkCheck            int

	// Extra anticheat checks (issue #43 analysis). All reuse data already on hand.
	//   ReactionMinMs      - human reaction floor (ms). #1 fast_reaction: flags a scoring
	//                        click whose arm→click latency (the first ScoredClick's OffsetMs)
	//                        is below this — catches a one-shot bot that fast_clicks (which
	//                        needs ≥2 scoring clicks) structurally can't. Also the touch→click
	//                        dwell floor for fast_hover (see TouchCheck). 0 = off.
	//   ImpossibleLatency  - >0 enables impossible_latency (#2): a scoring click whose
	//                        OffsetMs is below that connection's own min observed ping RTT is
	//                        physically impossible (the armed frame couldn't have arrived and a
	//                        click returned in the time). Per-connection, self-calibrating; the
	//                        hub supplies each conn's min RTT. 0 = off.
	//   MetronomeMinClicks - min scoring clicks (per player, per round) before the metronome
	//                        cadence check (#3) runs; MetronomeMaxCV is the coefficient-of-
	//                        variation ceiling below which the cadence is machine-flat. 0
	//                        MinClicks = off. Default off (opt-in: needs tuning per crowd).
	//   TouchCheck         - >0 enables the touch-derived check (#stretch/#5): fast_hover
	//                        (touch→click dwell below ReactionMinMs). The client sends
	//                        `touch {id}` on first entry into a live button. 0 = off. (The
	//                        old no_hover sub-check was dropped — per-frame touch sampling vs
	//                        immediate clicks false-tripped it; `busted` covers the egregious
	//                        scored-with-no-cursor case instead.)
	//   StraightPathRatio  - >0 enables straight_path (#6, signal-only, false-positive-prone so
	//                        default OFF): flags a window whose cursor path is implausibly
	//                        straight (net displacement / total path length above this ratio)
	//                        across ≥ StraightPathMinSamples points. 0 = off.
	ReactionMinMs      int
	ImpossibleLatency  int
	MetronomeMinClicks int
	MetronomeMaxCV     float64
	TouchCheck         int
	StraightPathRatio  float64
	StraightPathMin    int

	// Anticheat sanction ladder (per bounty, per player; see Engine.applySanction).
	// The first CheckCooldownThreshold-1 checks each bench the player behind a math
	// test. The CheckCooldownThreshold'th check starts a CheckCooldownMins cooldown
	// (clicks ignored, no test). CheckIgnoreAfter more checks past that sidelines
	// them until the bounty resolves. Counts reset when the bounty changes.
	//
	// A single round that trips ≥2 DISTINCT check types against one player is near-certain
	// automation, so applySanction bumps the ladder by the number of distinct types that
	// round, not just 1 (#4) — stacked evidence escalates faster.
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
		FastClickMs:         130,
		MaxClickFactor:      2.5,
		SoloLeadMargin:      4,
		DominantRunnerUpMin: 5,
		AfkCheck:            1,

		ReactionMinMs:      80,
		ImpossibleLatency:  1,
		MetronomeMinClicks: 0, // off by default — cadence check needs per-crowd tuning
		MetronomeMaxCV:     0.06,
		TouchCheck:         1,
		StraightPathRatio:  0, // off by default — false-positive-prone (issue #6)
		StraightPathMin:    12,

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
	c.FastClickMs = envInt("FAST_CLICK_MS", c.FastClickMs)
	c.MaxClickFactor = envFloat("MAX_CLICK_FACTOR", c.MaxClickFactor)
	c.SoloLeadMargin = envInt("SOLO_LEAD_MARGIN", c.SoloLeadMargin)
	c.DominantRunnerUpMin = envInt("DOMINANT_RUNNER_UP_MIN", c.DominantRunnerUpMin)
	c.AfkCheck = envInt("AFK_CHECK", c.AfkCheck)
	c.ReactionMinMs = envInt("REACTION_MIN_MS", c.ReactionMinMs)
	c.ImpossibleLatency = envInt("IMPOSSIBLE_LATENCY", c.ImpossibleLatency)
	c.MetronomeMinClicks = envInt("METRONOME_MIN_CLICKS", c.MetronomeMinClicks)
	c.MetronomeMaxCV = envFloat("METRONOME_MAX_CV", c.MetronomeMaxCV)
	c.TouchCheck = envInt("TOUCH_CHECK", c.TouchCheck)
	c.StraightPathRatio = envFloat("STRAIGHT_PATH_RATIO", c.StraightPathRatio)
	c.StraightPathMin = envInt("STRAIGHT_PATH_MIN", c.StraightPathMin)
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

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
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
