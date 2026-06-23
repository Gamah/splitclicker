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

// tempgunImage is the single placeholder weapon image served for every skin.
// Real CS2 skin images (whether uploaded or resolved from Valve's backend) are
// no longer surfaced; the client shows this in their place. Lives in the media dir.
const tempgunImage = "tempgun.png"

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
	inspect := ""
	hasBounty := false
	if b, ok, err := h.store.ActiveBounty(r.Context()); err != nil {
		h.log.Error("config: active bounty", zap.Error(err))
	} else if ok {
		hasBounty = true
		lock = b.WinTime.UnixMilli()
		inspect = b.InspectLink
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"winner_lock_ms": lock,
		"skin_url":       "/api/v1/skin",
		// The active bounty's CS2 inspect link, or "" when the skin is an uploaded
		// image only. The client decodes it locally and renders the live float /
		// seed / name / wear bar, falling back to skin_url's image on any failure.
		"inspect_link": inspect,
		// Whether a bounty is actually active. False ⇒ the winner_lock_ms/skin_url are
		// the host config.json/env fallback (a stale "old" skin) and the bounty-scoped
		// boards have fallen back to all-time data — the client shows a "no active
		// bounty" state instead of presenting that stale skin/countdown as live.
		"has_bounty": hasBounty,
	})
}

// GET /api/v1/skin — the current "skin to win" image. Always the tempgun
// placeholder: real bounty/Valve skin images are no longer surfaced. Served off
// disk from the media dir. ServeFile sets content-type and validation headers.
func (h *handler) skin(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join(runtimecfg.MediaDir(), tempgunImage))
}
