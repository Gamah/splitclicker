// Package runtimecfg loads the host-editable runtime config that lives in a
// bind-mounted directory (RUNTIME_DIR, default "data") alongside the skin images:
//
//	<RUNTIME_DIR>/media/        the servable images (also where admin uploads land)
//	<RUNTIME_DIR>/config.json   game tunables + fallback skin/winner-lock time
//
// The live skin and winner-lock countdown are now driven by the DB-managed
// bounty queue (see store.Bounty + the admin panel); config.json's SkinImage /
// WinnerLockTime are only the fallback used when no bounty is active.
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

	// LiveVersion is the current "live" client API version. A client whose version
	// is below this is treated as outdated (troll leaderboards + the out-of-date
	// dev note); live-or-newer is respected — so a new build (e.g. v3) can be tested
	// before the old one is disabled. Re-read per request, so it can be toggled live.
	// Absent ⇒ the api package's built-in default.
	LiveVersion *int `json:"live_version"`

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
	TickHz          *int `json:"tick_hz"`
	TickSampleK     *int `json:"tick_sample_k"`
	PenaltyBaseMs   *int `json:"penalty_base_ms"`
	PenaltyStepMs   *int `json:"penalty_step_ms"`
	FastClickMs     *int `json:"fast_click_ms"`
	MaxClickFactor  *int `json:"max_click_factor"`

	// Anticheat check gating + the per-bounty sanction ladder. See game.Config.
	SoloLeadMargin         *int `json:"solo_lead_margin"`
	DominantRunnerUpMin    *int `json:"dominant_runner_up_min"`
	CheckCooldownThreshold *int `json:"check_cooldown_threshold"`
	CheckCooldownMins      *int `json:"check_cooldown_mins"`
	CheckIgnoreAfter       *int `json:"check_ignore_after"`
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
