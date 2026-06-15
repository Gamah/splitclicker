package game

import (
	"os"
	"strconv"
	"time"
)

// Config holds the game tunables. All are server-side; see ConfigFromEnv.
type Config struct {
	ArmMin         time.Duration // shortest arming delay
	ArmMax         time.Duration // longest arming delay
	ClicksPerRound int           // N: scoring clicks per arm (1 = first-click-wins)
	RoundsPerGame  int           // X: rounds before game_over
	RaceMax        time.Duration // safety cap: close the race even if < N clicks land
	ResultDisplay  time.Duration // how long the round leaderboard shows
	Intermission   time.Duration // pause between games

	IdlePenaltyPerClick time.Duration // arm-delay penalty added per idle click
	IdlePenaltyCap      time.Duration // max accumulated penalty per round

	BoardSize int // top-K standings included in result/game_over frames
}

// DefaultConfig is the baseline tuning from PLAN.md §1.
func DefaultConfig() Config {
	return Config{
		ArmMin:              10 * time.Second,
		ArmMax:              120 * time.Second,
		ClicksPerRound:      1,
		RoundsPerGame:       10,
		RaceMax:             5 * time.Second,
		ResultDisplay:       4 * time.Second,
		Intermission:        5 * time.Second,
		IdlePenaltyPerClick: 10 * time.Millisecond,
		IdlePenaltyCap:      200 * time.Millisecond,
		BoardSize:           20,
	}
}

// ConfigFromEnv starts from DefaultConfig and overrides any tunable whose env
// var is set. Durations ending in Sec/Ms are read as integers in that unit.
func ConfigFromEnv() Config {
	c := DefaultConfig()
	c.ArmMin = envDur("ARM_MIN_SEC", c.ArmMin, time.Second)
	c.ArmMax = envDur("ARM_MAX_SEC", c.ArmMax, time.Second)
	c.ClicksPerRound = envInt("CLICKS_PER_ROUND", c.ClicksPerRound)
	c.RoundsPerGame = envInt("ROUNDS_PER_GAME", c.RoundsPerGame)
	c.RaceMax = envDur("RACE_MAX_MS", c.RaceMax, time.Millisecond)
	c.ResultDisplay = envDur("RESULT_DISPLAY_MS", c.ResultDisplay, time.Millisecond)
	c.Intermission = envDur("INTERMISSION_MS", c.Intermission, time.Millisecond)
	c.IdlePenaltyPerClick = envDur("IDLE_PENALTY_PER_CLICK_MS", c.IdlePenaltyPerClick, time.Millisecond)
	c.IdlePenaltyCap = envDur("IDLE_PENALTY_CAP_MS", c.IdlePenaltyCap, time.Millisecond)
	c.BoardSize = envInt("BOARD_SIZE", c.BoardSize)
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
