package api

import (
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gamah/splitclicker/internal/runtimecfg"
	"go.uber.org/zap"
)

// The skin-to-win image and the winner-lock countdown come from the active
// bounty (DB-managed via the admin panel — the skin currently in play and its
// win_time). When no bounty is active they fall back to the host-editable
// config.json (re-read per request, see package runtimecfg), then env, then the
// built-in default — so a fresh deploy with no bounties still serves a skin.

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
// current skin image. The lock time is the active bounty's win_time, falling
// back to config.json/env when no bounty is active.
func (h *handler) config(w http.ResponseWriter, r *http.Request) {
	lock := winnerLockMs()
	if b, ok, err := h.store.ActiveBounty(r.Context()); err != nil {
		h.log.Error("config: active bounty", zap.Error(err))
	} else if ok {
		lock = b.WinTime.UnixMilli()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"winner_lock_ms": lock,
		"skin_url":       "/api/v1/skin",
	})
}

// GET /api/v1/skin — the current skin image: the active bounty's skin_image,
// falling back to config.json/SKIN_IMAGE/default. Served off disk from the media
// dir (base-named so it can't traverse out). ServeFile sets content-type and
// validation headers; reading per request means a bounty swap applies live.
func (h *handler) skin(w http.ResponseWriter, r *http.Request) {
	name := skinFile()
	if b, ok, err := h.store.ActiveBounty(r.Context()); err != nil {
		h.log.Error("skin: active bounty", zap.Error(err))
	} else if ok && b.SkinImage != "" {
		name = filepath.Base(b.SkinImage)
	}
	http.ServeFile(w, r, filepath.Join(runtimecfg.MediaDir(), name))
}
