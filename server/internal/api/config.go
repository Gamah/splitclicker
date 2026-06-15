package api

import (
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gamah/splitclicker/internal/runtimecfg"
)

// The skin-to-win image and the winner-lock countdown are editable from the host
// filesystem at runtime — no rebuild, no restart. Both live in config.json under
// the host-mounted runtime dir (see package runtimecfg), which is re-read on every
// request: editing it on the host applies immediately. A missing file or empty
// field falls back to env, then to the built-in default.

// skinFile is the image served as the current skin: config.json's skin_image,
// else env SKIN_IMAGE, else the default. Base-named so it can't traverse out of
// the media dir.
func skinFile() string {
	f := runtimecfg.Load().SkinImage
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
	v := runtimecfg.Load().WinnerLockTime
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
// and served off disk from the media dir. ServeFile sets content-type and
// validation headers; reading per request means replacing the file is live.
func (h *handler) skin(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join(runtimecfg.MediaDir(), skinFile()))
}
