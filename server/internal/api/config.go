package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// The skin-to-win image and the winner-lock countdown are editable from the host
// filesystem at runtime — no rebuild, no restart. Both live under a host-mounted
// directory (RUNTIME_DIR, default "data"):
//
//	<RUNTIME_DIR>/media/        the image files (replace one to change the skin bytes)
//	<RUNTIME_DIR>/config.json   { "skin_image": "...", "winner_lock_time": "<RFC3339>" }
//
// config.json is re-read on every request, so editing it on the host applies
// immediately. A missing file or empty field falls back to env, then to the
// built-in default — so the env-only setup still works.

// runtimeDir is the host-mounted config+media directory. The Docker image sets
// RUNTIME_DIR=/data and bind-mounts the host's ./data there.
func runtimeDir() string {
	if d := os.Getenv("RUNTIME_DIR"); d != "" {
		return d
	}
	return "data"
}

// mediaDir is where servable images live: <RUNTIME_DIR>/media.
func mediaDir() string {
	return filepath.Join(runtimeDir(), "media")
}

// hostConfig mirrors RUNTIME_DIR/config.json, the host-editable runtime config.
type hostConfig struct {
	SkinImage      string `json:"skin_image"`
	WinnerLockTime string `json:"winner_lock_time"`
}

// readHostConfig loads config.json fresh on each call (so host edits apply without
// a restart). A missing or malformed file yields the zero value, never an error.
func readHostConfig() hostConfig {
	var c hostConfig
	b, err := os.ReadFile(filepath.Join(runtimeDir(), "config.json"))
	if err != nil {
		return c
	}
	_ = json.Unmarshal(b, &c)
	return c
}

// skinFile is the image served as the current skin: config.json's skin_image,
// else env SKIN_IMAGE, else the default. Base-named so it can't traverse out of
// mediaDir.
func skinFile() string {
	f := readHostConfig().SkinImage
	if f == "" {
		f = os.Getenv("SKIN_IMAGE")
	}
	if f == "" {
		f = "skin2win.png"
	}
	return filepath.Base(f)
}

// winnerLockMs is when the current winner is locked in, as Unix epoch ms (0 =
// unset/unparseable): config.json's winner_lock_time, else env WINNER_LOCK_TIME.
// Sent as a plain number so the s&box client needs no date parsing (its sandbox
// doesn't whitelist System.Globalization).
func winnerLockMs() int64 {
	v := readHostConfig().WinnerLockTime
	if v == "" {
		v = os.Getenv("WINNER_LOCK_TIME")
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// GET /api/v1/config — the small bit of public config the client needs at
// startup: the winner-lock time (drives the countdown) and where to fetch the
// current skin image.
func (h *handler) config(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"winner_lock_ms": winnerLockMs(),
		"skin_url":       "/api/v1/skin",
	})
}

// GET /api/v1/skin — the current skin image, selected via config.json/SKIN_IMAGE
// and served off disk from mediaDir. ServeFile sets content-type and validation
// headers; because it reads the file per request, replacing it on the host is live.
func (h *handler) skin(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join(mediaDir(), skinFile()))
}
