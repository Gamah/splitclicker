// Package runtimecfg loads the host-editable runtime config that lives in a
// bind-mounted directory (RUNTIME_DIR, default "data") alongside the skin images:
//
//	<RUNTIME_DIR>/media/        the servable images
//	<RUNTIME_DIR>/config.json   skin selection, winner-lock time, and game tunables
//
// It is plain stdlib (no project deps) so both the API layer (which re-reads it
// per request for the live skin/countdown) and main (which reads it once at
// startup for the game tunables) can use it.
package runtimecfg

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// File mirrors config.json. Numeric game tunables are pointers so "absent"
// (nil → fall back to env/default) is distinct from a real value; the two
// strings use "" as their absent sentinel.
type File struct {
	SkinImage      string `json:"skin_image"`
	WinnerLockTime string `json:"winner_lock_time"`

	// DevNote is a host-editable broadcast message; when non-empty, clients show
	// it (orange) under the throttle line until it is cleared. Read once per game.
	DevNote string `json:"dev_note"`

	// Game tunables (applied at startup; a `restart` reloads them). Units are in
	// the field names. See game.Config for meaning.
	ArmMinSec       *int `json:"arm_min_sec"`
	ArmMaxSec       *int `json:"arm_max_sec"`
	ClicksPerPlayer *int `json:"clicks_per_player"`
	MinClicks       *int `json:"min_clicks"`
	RoundsPerGame   *int `json:"rounds_per_game"`
	RaceMaxMs       *int `json:"race_max_ms"`
	ResultDisplayMs *int `json:"result_display_ms"`
	IntermissionMs  *int `json:"intermission_ms"`
	BoardSize       *int `json:"board_size"`
	PenaltyBaseMs   *int `json:"penalty_base_ms"`
	PenaltyStepMs   *int `json:"penalty_step_ms"`
}

// Dir is the host-mounted config+media directory.
func Dir() string {
	if d := os.Getenv("RUNTIME_DIR"); d != "" {
		return d
	}
	return "data"
}

// MediaDir is where servable images live: <Dir>/media.
func MediaDir() string { return filepath.Join(Dir(), "media") }

// Load reads config.json fresh; a missing or malformed file yields the zero
// File (every field absent), never an error.
func Load() File {
	var f File
	b, err := os.ReadFile(filepath.Join(Dir(), "config.json"))
	if err != nil {
		return f
	}
	_ = json.Unmarshal(b, &f)
	return f
}
