package api

import (
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/gamah/splitclicker/internal/runtimecfg"
	"go.uber.org/zap"
)

// GET /api/{ver}/bounties/previous — up to the 5 most recently settled bounties,
// newest first, each with its winner (public tag + steamid + name + games won) and
// its skin (inspect link and/or image URL). Drives the client's "previous winner"
// panel; the client re-fetches this (and /config) on load, on (re)connect, and
// whenever the server pushes a `bounty_update`. Soft-fails to an empty list so a
// DB blip just hides the panel rather than breaking the HUD.
func (h *handler) previousBounties(w http.ResponseWriter, r *http.Request) {
	bs, err := h.store.RecentWonBounties(r.Context(), 5)
	if err != nil {
		h.log.Error("previous bounties", zap.Error(err))
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	out := make([]map[string]any, 0, len(bs))
	for _, b := range bs {
		out = append(out, map[string]any{
			"id":    b.ID,
			"label": b.Label,
			// Per-bounty image route (v1 path works for any client version, mirroring
			// config's skin_url); inspect-link bounties resolve their image client-side
			// and only fall back to this.
			"skin_url":        "/api/v1/skin/" + strconv.FormatInt(b.ID, 10),
			"inspect_link":    b.InspectLink,
			"winner_tag":      b.WinnerTag,
			"winner_steam_id": b.WinnerID,
			"winner_name":     b.WinnerName,
			"winner_wins":     b.WinnerWins,
			"won_at_ms":       b.WonAt.UnixMilli(),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /api/{ver}/skin/{id} — the uploaded skin image for a specific bounty, so the
// previous-winner panel can show an image-only past skin. Inspect-link bounties
// resolve their image client-side; this is the fallback. Base-named so it can't
// traverse out of the media dir.
func (h *handler) skinByID(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	name, err := h.store.BountySkinImage(r.Context(), id)
	if err != nil {
		h.log.Error("skin by id", zap.Error(err))
		http.NotFound(w, r)
		return
	}
	if name == "" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(runtimecfg.MediaDir(), filepath.Base(name)))
}
