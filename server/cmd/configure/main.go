// Command configure is the interactive pre-build config review run by `make up`.
// It loads the host's data/config.json (seeding it from config.json.example when
// absent), walks every editable value showing the current setting, and lets the
// operator press Enter to keep it or type a new one. The result is written back to
// data/config.json — the file bind-mounted into the app container — before the
// build proceeds.
//
//	go run ./cmd/configure          # interactive review
//	go run ./cmd/configure --skip   # keep current/default values, no prompts
//
// It deliberately uses only the runtimecfg.File schema + stdlib, so it stays in
// lockstep with what the server actually reads.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gamah/splitclicker/internal/runtimecfg"
)

// field is one editable config.json value: its key, how to render the current
// value for the prompt, and how to parse a typed-in replacement onto the File.
type field struct {
	key string
	get func(*runtimecfg.File) string
	set func(*runtimecfg.File, string) error
}

// fields is the prompt order: the live meta block, then the startup game tunables
// (the ones that actually need a rebuild/restart to take effect).
var fields = []field{
	{"skin_image", func(f *runtimecfg.File) string { return f.SkinImage }, func(f *runtimecfg.File, s string) error { f.SkinImage = s; return nil }},
	{"winner_lock_time", func(f *runtimecfg.File) string { return f.WinnerLockTime }, func(f *runtimecfg.File, s string) error { f.WinnerLockTime = s; return nil }},
	{"dev_note", func(f *runtimecfg.File) string { return f.DevNote }, func(f *runtimecfg.File, s string) error { f.DevNote = s; return nil }},
	{"live_version", func(f *runtimecfg.File) string { return itoa(f.LiveVersion) }, func(f *runtimecfg.File, s string) error { return setInt(&f.LiveVersion, s) }},
	{"arm_min_sec", func(f *runtimecfg.File) string { return itoa(f.ArmMinSec) }, func(f *runtimecfg.File, s string) error { return setInt(&f.ArmMinSec, s) }},
	{"arm_max_sec", func(f *runtimecfg.File) string { return itoa(f.ArmMaxSec) }, func(f *runtimecfg.File, s string) error { return setInt(&f.ArmMaxSec, s) }},
	{"clicks_per_player", func(f *runtimecfg.File) string { return itoa(f.ClicksPerPlayer) }, func(f *runtimecfg.File, s string) error { return setInt(&f.ClicksPerPlayer, s) }},
	{"min_clicks", func(f *runtimecfg.File) string { return itoa(f.MinClicks) }, func(f *runtimecfg.File, s string) error { return setInt(&f.MinClicks, s) }},
	{"rounds_per_game", func(f *runtimecfg.File) string { return itoa(f.RoundsPerGame) }, func(f *runtimecfg.File, s string) error { return setInt(&f.RoundsPerGame, s) }},
	{"buttons_on_screen", func(f *runtimecfg.File) string { return itoa(f.ButtonsOnScreen) }, func(f *runtimecfg.File, s string) error { return setInt(&f.ButtonsOnScreen, s) }},
	{"race_max_ms", func(f *runtimecfg.File) string { return itoa(f.RaceMaxMs) }, func(f *runtimecfg.File, s string) error { return setInt(&f.RaceMaxMs, s) }},
	{"result_display_ms", func(f *runtimecfg.File) string { return itoa(f.ResultDisplayMs) }, func(f *runtimecfg.File, s string) error { return setInt(&f.ResultDisplayMs, s) }},
	{"intermission_ms", func(f *runtimecfg.File) string { return itoa(f.IntermissionMs) }, func(f *runtimecfg.File, s string) error { return setInt(&f.IntermissionMs, s) }},
	{"board_size", func(f *runtimecfg.File) string { return itoa(f.BoardSize) }, func(f *runtimecfg.File, s string) error { return setInt(&f.BoardSize, s) }},
	{"tick_hz", func(f *runtimecfg.File) string { return itoa(f.TickHz) }, func(f *runtimecfg.File, s string) error { return setInt(&f.TickHz, s) }},
	{"tick_sample_k", func(f *runtimecfg.File) string { return itoa(f.TickSampleK) }, func(f *runtimecfg.File, s string) error { return setInt(&f.TickSampleK, s) }},
	{"penalty_base_ms", func(f *runtimecfg.File) string { return itoa(f.PenaltyBaseMs) }, func(f *runtimecfg.File, s string) error { return setInt(&f.PenaltyBaseMs, s) }},
	{"penalty_step_ms", func(f *runtimecfg.File) string { return itoa(f.PenaltyStepMs) }, func(f *runtimecfg.File, s string) error { return setInt(&f.PenaltyStepMs, s) }},
	{"fast_click_ms", func(f *runtimecfg.File) string { return itoa(f.FastClickMs) }, func(f *runtimecfg.File, s string) error { return setInt(&f.FastClickMs, s) }},
	{"max_click_factor", func(f *runtimecfg.File) string { return ftoa(f.MaxClickFactor) }, func(f *runtimecfg.File, s string) error { return setFloat(&f.MaxClickFactor, s) }},
	{"solo_lead_margin", func(f *runtimecfg.File) string { return itoa(f.SoloLeadMargin) }, func(f *runtimecfg.File, s string) error { return setInt(&f.SoloLeadMargin, s) }},
	{"dominant_runner_up_min", func(f *runtimecfg.File) string { return itoa(f.DominantRunnerUpMin) }, func(f *runtimecfg.File, s string) error { return setInt(&f.DominantRunnerUpMin, s) }},
	{"check_cooldown_threshold", func(f *runtimecfg.File) string { return itoa(f.CheckCooldownThreshold) }, func(f *runtimecfg.File, s string) error { return setInt(&f.CheckCooldownThreshold, s) }},
	{"check_cooldown_mins", func(f *runtimecfg.File) string { return itoa(f.CheckCooldownMins) }, func(f *runtimecfg.File, s string) error { return setInt(&f.CheckCooldownMins, s) }},
	{"check_ignore_after", func(f *runtimecfg.File) string { return itoa(f.CheckIgnoreAfter) }, func(f *runtimecfg.File, s string) error { return setInt(&f.CheckIgnoreAfter, s) }},
}

func main() {
	skip := flag.Bool("skip", false, "keep current/default config values without prompting")
	flag.Parse()

	dir := runtimecfg.Dir()
	path := filepath.Join(dir, "config.json")
	examplePath := filepath.Join(dir, "config.json.example")

	// Start from the example (so every key has a sane default), then overlay any
	// values already in config.json. Keys absent from config.json keep the example
	// default — and get written back, so a partial file is filled in.
	cfg, err := loadFile(examplePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure: cannot read %s: %v\n", examplePath, err)
		os.Exit(1)
	}
	existed := overlay(&cfg, path)

	if *skip {
		if existed {
			fmt.Printf("configure: --skip, keeping current %s\n", path)
			return
		}
		if err := writeFile(path, &cfg); err != nil {
			fmt.Fprintf(os.Stderr, "configure: write %s: %v\n", path, err)
			os.Exit(1)
		}
		fmt.Printf("configure: --skip, seeded %s from example defaults\n", path)
		return
	}

	fmt.Printf("Reviewing %s — press Enter to keep the [current] value, or type a new one.\n\n", path)
	in := bufio.NewReader(os.Stdin)
	for _, fl := range fields {
		for {
			fmt.Printf("  %s [%s]: ", fl.key, fl.get(&cfg))
			line, err := in.ReadString('\n')
			s := strings.TrimSpace(line)
			if s == "" { // blank (or EOF with no input) keeps the current value
				break
			}
			if e := fl.set(&cfg, s); e != nil {
				fmt.Printf("    invalid value (%v) — try again\n", e)
				if err != nil { // EOF: don't loop forever on a closed stdin
					break
				}
				continue
			}
			break
		}
	}

	if err := writeFile(path, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "\nconfigure: write %s: %v\n", path, err)
		os.Exit(1)
	}
	fmt.Printf("\nconfigure: wrote %s\n", path)
}

// loadFile reads a config JSON into a File.
func loadFile(path string) (runtimecfg.File, error) {
	var f runtimecfg.File
	b, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	return f, json.Unmarshal(b, &f)
}

// overlay unmarshals config.json (if present) onto an already-populated File,
// replacing only the keys it contains. Returns whether the file existed.
func overlay(f *runtimecfg.File, path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	_ = json.Unmarshal(b, f)
	return true
}

// writeFile marshals the File back to pretty JSON with a trailing newline.
func writeFile(path string, f *runtimecfg.File) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func itoa(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}

func ftoa(p *float64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatFloat(*p, 'g', -1, 64)
}

func setInt(p **int, s string) error {
	n, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	v := n
	*p = &v
	return nil
}

func setFloat(p **float64, s string) error {
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	v := n
	*p = &v
	return nil
}
