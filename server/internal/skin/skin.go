// Package skin server-side resolves a CS2 inspect link to the weapon's skin
// image, so a bounty created from an inspect link gets a stored fallback image
// (served by /api/v1/skin) even when a client can't decode the link itself.
//
// It mirrors client/Code/Game/SkinInspect.cs: since Valve's March 2026 change an
// inspect link self-encodes the item, so the weapon/paint ids come straight out
// of the link's protobuf with no Steam call. The human name + weapon image are
// NOT in the link, so they're looked up by (defindex, paintindex) in the
// community ByMykel/CSGO-API dataset (fetched once, cached in-process). Any
// failure leaves the caller to keep whatever fallback image it already had.
package skin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	datasetURL = "https://raw.githubusercontent.com/ByMykel/CSGO-API/main/public/api/en/skins.json"
	// datasetTTL is how long the parsed (defindex,paintindex)→image index is reused
	// before a refetch. Admin saves are rare, so a day keeps it current cheaply.
	datasetTTL = 24 * time.Hour
	maxImage   = 8 << 20 // 8 MiB, mirroring the admin upload cap
)

// client has a bounded timeout so a slow dataset/image host can't hang an admin
// save indefinitely (the caller also passes a request-scoped deadline).
var client = &http.Client{Timeout: 15 * time.Second}

// allowedExt is the set of image extensions a downloaded skin image may use; an
// unrecognised one is stored as .png (the dataset's images are PNGs).
var allowedExt = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
}

// Resolve decodes link, finds its weapon image in the dataset, downloads it, and
// returns the image bytes plus a filename extension (e.g. ".png"). ok is false on
// any non-fatal miss (malformed link, unknown skin) with err nil; err is non-nil
// only for an actual fetch/transport failure. On either, the caller keeps its
// existing fallback — a link-only bounty still works (the client decodes it).
func Resolve(ctx context.Context, link string) (data []byte, ext string, ok bool, err error) {
	def, paint, dok := Decode(link)
	if !dok {
		return nil, "", false, nil
	}
	idx, err := loadIndex(ctx)
	if err != nil {
		return nil, "", false, err
	}
	imgURL := idx[key(def, paint)]
	if imgURL == "" {
		return nil, "", false, nil
	}
	data, ext, err = download(ctx, imgURL)
	if err != nil {
		return nil, "", false, err
	}
	return data, ext, true, nil
}

// ── link decoding (port of SkinInspect.Decode) ─────────────────────────────

// Decode parses an inspect link (a full steam://… link or a bare hex payload)
// into its weapon/paint indices. ok is false on anything malformed.
func Decode(link string) (defIndex, paintIndex int, ok bool) {
	hex, hok := extractHex(link)
	if !hok {
		return 0, 0, false
	}
	buf, bok := hexToBytes(hex)
	if !bok || len(buf) < 6 {
		return 0, 0, false
	}
	// Masked links are XOR'd with their own first byte (so the real leading 0x00
	// becomes key^key=0 on the wire); unmask in place if so.
	if buf[0] != 0 {
		k := buf[0]
		for i := range buf {
			buf[i] ^= k
		}
	}
	if buf[0] != 0 {
		return 0, 0, false // must start with the 0x00 marker once unmasked
	}
	// Payload sits between the leading 0x00 and the trailing 4-byte CRC.
	defIndex, paintIndex = parseProto(buf[1 : len(buf)-4])
	if defIndex == 0 || paintIndex == 0 {
		return 0, 0, false
	}
	return defIndex, paintIndex, true
}

// extractHex pulls the trailing hex token out of whatever was pasted: a full
// steam://…+csgo_econ_action_preview%20<HEX> link, the space-separated form, or
// just the bare hex. ok is false when no usable hex is found.
func extractHex(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	s = strings.ReplaceAll(s, "%20", " ")
	if sp := strings.LastIndex(s, " "); sp >= 0 {
		s = s[sp+1:]
	}
	s = strings.TrimSpace(s)
	if len(s) < 12 || len(s)%2 != 0 {
		return "", false
	}
	for _, c := range s {
		if hexVal(c) < 0 {
			return "", false
		}
	}
	return s, true
}

func hexToBytes(s string) ([]byte, bool) {
	b := make([]byte, len(s)/2)
	for i := range b {
		hi, lo := hexVal(rune(s[i*2])), hexVal(rune(s[i*2+1]))
		if hi < 0 || lo < 0 {
			return nil, false
		}
		b[i] = byte(hi<<4 | lo)
	}
	return b, true
}

func hexVal(c rune) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// parseProto reads the only protobuf fields we need (defindex=3, paintindex=4)
// and skips everything else by wire type. It never indexes out of range: every
// read is bounded by end and an over-long length-delimited skip just ends the loop.
func parseProto(b []byte) (defIndex, paintIndex int) {
	i, end := 0, len(b)
	for i < end {
		tag, ok := readVarint(b, &i, end)
		if !ok {
			return
		}
		field := int(tag >> 3)
		switch tag & 7 {
		case 0: // varint
			v, ok := readVarint(b, &i, end)
			if !ok {
				return
			}
			switch field {
			case 3:
				defIndex = int(v)
			case 4:
				paintIndex = int(v)
			}
		case 1: // 64-bit
			i += 8
		case 5: // 32-bit
			i += 4
		case 2: // length-delimited → skip
			ln, ok := readVarint(b, &i, end)
			if !ok || ln > uint64(end-i) {
				return
			}
			i += int(ln)
		default: // groups / unknown wire type → bail
			return
		}
	}
	return
}

func readVarint(b []byte, i *int, end int) (uint64, bool) {
	var result uint64
	var shift uint
	for *i < end && shift < 64 {
		c := b[*i]
		*i++
		result |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return result, true
		}
		shift += 7
	}
	return 0, false
}

// ── dataset index (port of SkinInspect.LoadIndex / BuildIndex) ──────────────

func key(defIndex, paintIndex int) int64 { return int64(defIndex)<<20 | int64(uint32(paintIndex)) }

var (
	idxMu      sync.Mutex
	idxCache   map[int64]string // (defindex,paintindex) → weapon image URL
	idxFetched time.Time
)

// loadIndex returns the dataset index, refetching when missing or older than the
// TTL. A refetch failure with a cached index falls back to the stale copy rather
// than failing the resolve outright.
func loadIndex(ctx context.Context) (map[int64]string, error) {
	idxMu.Lock()
	defer idxMu.Unlock()
	if idxCache != nil && time.Since(idxFetched) < datasetTTL {
		return idxCache, nil
	}
	built, err := buildIndex(ctx)
	if err != nil {
		if idxCache != nil {
			return idxCache, nil // stale-but-usable beats failing the save
		}
		return nil, err
	}
	idxCache = built
	idxFetched = time.Now()
	return idxCache, nil
}

func buildIndex(ctx context.Context) (map[int64]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, datasetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("skin: build dataset request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("skin: fetch dataset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("skin: dataset status %d", resp.StatusCode)
	}
	var list []rawSkin
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("skin: decode dataset: %w", err)
	}
	index := make(map[int64]string, len(list))
	for _, s := range list {
		if s.Image == "" || s.PaintIndex == "" {
			continue
		}
		pi, err := strconv.Atoi(s.PaintIndex)
		if err != nil {
			continue
		}
		index[key(s.Weapon.WeaponID, pi)] = s.Image
	}
	return index, nil
}

// rawSkin is just the dataset fields we use; everything else is ignored.
type rawSkin struct {
	Image      string `json:"image"`
	PaintIndex string `json:"paint_index"`
	Weapon     struct {
		WeaponID int `json:"weapon_id"`
	} `json:"weapon"`
}

// ── image download ──────────────────────────────────────────────────────────

func download(ctx context.Context, url string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("skin: build image request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("skin: fetch image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("skin: image status %d", resp.StatusCode)
	}
	// LimitReader to maxImage+1 so an over-cap image is detected, not silently
	// truncated into a corrupt file.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImage+1))
	if err != nil {
		return nil, "", fmt.Errorf("skin: read image: %w", err)
	}
	if len(data) > maxImage {
		return nil, "", fmt.Errorf("skin: image exceeds %d MiB", maxImage>>20)
	}
	return data, extFor(url), nil
}

// extFor returns the lower-cased image extension from a URL's path, defaulting to
// .png (the dataset serves PNGs) for anything unrecognised.
func extFor(url string) string {
	if i := strings.IndexByte(url, '?'); i >= 0 {
		url = url[:i]
	}
	ext := strings.ToLower(path.Ext(url))
	if allowedExt[ext] {
		return ext
	}
	return ".png"
}
