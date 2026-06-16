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
	RoundsPerGame   int           // X: rounds before game_over
	RaceMax       time.Duration // safety cap: close the race even if < N clicks land
	ResultDisplay time.Duration // how long the round leaderboard shows
	Intermission  time.Duration // pause between games

	BoardSize int // top-K standings included in result/game_over frames

	// Bad-click penalty escalation (ms): the kth bad click since the last arm adds
	// PenaltyBaseMs + PenaltyStepMs·(k−1) to that connection's held arm delay. Sent
	// to clients on connect so they mirror the live estimate. See idlePenalty.
	PenaltyBaseMs int
	PenaltyStepMs int
}

// DefaultConfig is the baseline tuning (overridable via data/config.json, then env).
func DefaultConfig() Config {
	return Config{
		ArmMin:          2 * time.Second,
		ArmMax:          6 * time.Second,
		ClicksPerPlayer: 15,
		MinClicks:       50,
		RoundsPerGame:   5,
		RaceMax:         5 * time.Second,
		ResultDisplay:   4 * time.Second,
		Intermission:    5 * time.Second,
		BoardSize:       20,
		PenaltyBaseMs:   500,
		PenaltyStepMs:   100,
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
	c.RaceMax = envDur("RACE_MAX_MS", c.RaceMax, time.Millisecond)
	c.ResultDisplay = envDur("RESULT_DISPLAY_MS", c.ResultDisplay, time.Millisecond)
	c.Intermission = envDur("INTERMISSION_MS", c.Intermission, time.Millisecond)
	c.BoardSize = envInt("BOARD_SIZE", c.BoardSize)
	c.PenaltyBaseMs = envInt("PENALTY_BASE_MS", c.PenaltyBaseMs)
	c.PenaltyStepMs = envInt("PENALTY_STEP_MS", c.PenaltyStepMs)
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
