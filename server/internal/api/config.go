package api

import (
	"net/http"
	"os"
	"path/filepath"
)

// The "skin to win" image and the winner-lock countdown are both server-driven so
// they can be retuned without shipping a new client build: the image is selected
// by env and served straight off disk, and the lock time is just a string the
// client counts down to.

// mediaDir is where servable images live. Defaults to ./media (so `make dev` run
// from server/ works); the Docker image sets MEDIA_DIR=/media.
func mediaDir() string {
	if d := os.Getenv("MEDIA_DIR"); d != "" {
		return d
	}
	return "media"
}

// skinFile is the image served as the current "skin to win", selectable via
// SKIN_IMAGE (default skin2win.png). Base-named so it can't traverse out of
// mediaDir.
func skinFile() string {
	f := os.Getenv("SKIN_IMAGE")
	if f == "" {
		f = "skin2win.png"
	}
	return filepath.Base(f)
}

// winnerLockTime is when the current winner is locked in (RFC3339, set via
// WINNER_LOCK_TIME). Passed through verbatim; the client parses and counts down
// to it, and once past prompts Gamah for a new skin.
func winnerLockTime() string {
	return os.Getenv("WINNER_LOCK_TIME")
}

// GET /api/v1/config — the small bit of public config the client needs at
// startup: the winner-lock time (drives the countdown) and where to fetch the
// current skin image.
func (h *handler) config(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"winner_lock_time": winnerLockTime(),
		"skin_url":         "/api/v1/skin",
	})
}

// GET /api/v1/skin — the current skin image, selected via SKIN_IMAGE and served
// off disk from mediaDir. ServeFile sets content-type and validation headers.
func (h *handler) skin(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join(mediaDir(), skinFile()))
}
