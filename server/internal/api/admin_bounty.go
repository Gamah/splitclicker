package api

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gamah/splitclicker/internal/runtimecfg"
)

// maxSkinUpload caps an uploaded skin image (generous for a PNG, ruinous for an
// attempt to fill the disk).
const maxSkinUpload = 8 << 20 // 8 MiB

// allowedSkinExt is the set of image extensions an uploaded skin may use.
var allowedSkinExt = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
}

// skinNameUnsafe matches characters not allowed in a stored skin filename; they
// are collapsed to "_" so the name stays a safe single path segment.
var skinNameUnsafe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// POST /admin/bounties — add a pending bounty. Multipart: an uploaded skin
// image (required), a label, and the win_time (datetime-local, read as UTC).
func (h *handler) adminBountyCreate(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuth(w, r) {
		return
	}
	if err := parseAdminUpload(w, r); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	winTime, ok := parseAdminTime(r.FormValue("win_time"))
	if !ok {
		http.Error(w, "invalid or missing win_time", http.StatusBadRequest)
		return
	}
	image, err := saveSkinUpload(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	link := strings.TrimSpace(r.FormValue("inspect_link"))
	if image == "" && link == "" {
		http.Error(w, "provide a skin image or an inspect link", http.StatusBadRequest)
		return
	}
	if err := h.store.CreateBounty(r.Context(), image, link, strings.TrimSpace(r.FormValue("label")), winTime); err != nil {
		h.adminError(w, "create bounty", err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// POST /admin/bounties/edit — edit a not-yet-won bounty's label, win_time, and
// (optionally) its skin image. Omitting the file keeps the current image.
func (h *handler) adminBountyEdit(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuth(w, r) {
		return
	}
	if err := parseAdminUpload(w, r); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	winTime, ok := parseAdminTime(r.FormValue("win_time"))
	if !ok {
		http.Error(w, "invalid or missing win_time", http.StatusBadRequest)
		return
	}
	image, err := saveSkinUpload(r) // "" when no new file was chosen
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	link := strings.TrimSpace(r.FormValue("inspect_link"))
	if err := h.store.UpdateBounty(r.Context(), id, image, link, strings.TrimSpace(r.FormValue("label")), winTime); err != nil {
		h.adminError(w, "update bounty", err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// POST /admin/bounties/delete — remove a pending bounty.
func (h *handler) adminBountyDelete(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuth(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.store.DeleteBounty(r.Context(), id); err != nil {
		h.adminError(w, "delete bounty", err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// GET /admin/media?f=<name> — serve a media-dir image for the admin queue
// thumbnails. Admin-gated and base-named so it can't traverse out of the dir.
func (h *handler) adminMedia(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuth(w, r) {
		return
	}
	name := filepath.Base(r.URL.Query().Get("f"))
	if name == "" || name == "." || name == "/" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(runtimecfg.MediaDir(), name))
}

// parseAdminUpload caps the request body and parses the multipart form. It must
// run before any form value is read so the size limit actually applies (the
// limit is on the whole body; ParseMultipartForm's own arg only governs the
// in-memory/spill threshold). Call once per request, before parseAdminTime etc.
func parseAdminUpload(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxSkinUpload)
	if err := r.ParseMultipartForm(maxSkinUpload); err != nil {
		return fmt.Errorf("upload too large (max %d MiB) or malformed", maxSkinUpload>>20)
	}
	return nil
}

// saveSkinUpload reads the optional "skin" file from the (already-parsed)
// multipart form, validates its extension, writes it to the media dir under a
// collision-proof name, and returns that base filename. It returns "" (no error)
// when no file was uploaded, so edit can leave the existing image untouched.
func saveSkinUpload(r *http.Request) (string, error) {
	file, hdr, err := r.FormFile("skin")
	if err == http.ErrMissingFile {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("could not read upload")
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(hdr.Filename))
	if !allowedSkinExt[ext] {
		return "", fmt.Errorf("unsupported image type %q (png, jpg, gif, webp)", ext)
	}

	// Unique, traversal-safe filename: <unixnano>_<sanitized original base>.
	base := skinNameUnsafe.ReplaceAllString(filepath.Base(hdr.Filename), "_")
	name := fmt.Sprintf("%d_%s", time.Now().UnixNano(), base)

	if err := os.MkdirAll(runtimecfg.MediaDir(), 0o755); err != nil {
		return "", fmt.Errorf("could not prepare media dir")
	}
	dst, err := os.Create(filepath.Join(runtimecfg.MediaDir(), name))
	if err != nil {
		return "", fmt.Errorf("could not save image")
	}
	defer dst.Close()
	// Cap the copy as a second line of defense behind ParseMultipartForm.
	if _, err := io.Copy(dst, io.LimitReader(file, maxSkinUpload)); err != nil {
		return "", fmt.Errorf("could not write image")
	}
	return name, nil
}

// parseAdminTime parses an HTML datetime-local value (no zone) as UTC, so it
// matches how the admin views render timestamps. Both with- and without-seconds
// forms are accepted. ok is false on empty/unparseable input.
func parseAdminTime(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, v, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
