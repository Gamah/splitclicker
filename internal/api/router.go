// Package api wires the HTTP REST surface and the WebSocket upgrade.
//
// The surface is deliberately tiny: prove a Steam identity once (POST /auth),
// get a one-time WS ticket, connect the socket, and read the hourly board. All
// realtime game traffic rides the WebSocket (package ws), never HTTP.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/gamah/splitclicker/internal/game"
	"github.com/gamah/splitclicker/internal/session"
	"github.com/gamah/splitclicker/internal/steam"
	"github.com/gamah/splitclicker/internal/store"
	"github.com/gamah/splitclicker/internal/ws"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"
)

type handler struct {
	store    *store.Store
	hub      *ws.Hub
	engine   *game.Engine
	log      *zap.Logger
	tickets  *ticketStore
	upgrader websocket.Upgrader
}

// allowedOrigins is the cross-origin allowlist for the REST CORS middleware and
// the WS upgrader. Non-browser clients (the s&box client) send no Origin and are
// allowed; a present Origin must be same-host or in the allowlist.
var allowedOrigins = map[string]bool{}

func initOrigins() {
	for _, o := range strings.Split(os.Getenv("CORS_ALLOWED_ORIGINS"), ",") {
		o = strings.TrimRight(strings.TrimSpace(o), "/")
		if o != "" {
			allowedOrigins[o] = true
		}
	}
}

func checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser client (s&box)
	}
	if allowedOrigins[strings.TrimRight(origin, "/")] {
		return true
	}
	if u, err := url.Parse(origin); err == nil && u.Host == r.Host {
		return true
	}
	return false
}

// NewRouter builds the HTTP mux.
func NewRouter(st *store.Store, hub *ws.Hub, engine *game.Engine, log *zap.Logger) *http.ServeMux {
	initOrigins()
	mux := http.NewServeMux()

	// ~1 req/sec sustained, burst 5 — generous for a human, ruinous for a loop.
	rl := newRateLimiter(1, 5)

	h := &handler{
		store:   st,
		hub:     hub,
		engine:  engine,
		log:     log,
		tickets: newTicketStore(),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin:     checkOrigin,
		},
	}

	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("POST /api/v1/auth", rl.wrap(h.auth))
	mux.HandleFunc("GET /api/v1/leaderboard/hourly", h.hourlyLeaderboard)
	mux.HandleFunc("GET /ws", h.wsConnect)

	return mux
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// steamIDRe matches a SteamID64 (1–20 digits). Stored as TEXT, never used in
// arithmetic, so a digit check is sufficient validation.
var steamIDRe = regexp.MustCompile(`^[0-9]{1,20}$`)

// POST /api/v1/auth  body: {"steam_id":"765…","token":"…","username":"optional"}
//
// Validates the Facepunch token server-side (fail-closed), upserts the player,
// and returns the public tag/username plus a single-use WS ticket. This is the
// only identity step — there is no Steam OpenID web sign-in.
func (h *handler) auth(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SteamID  string `json:"steam_id"`
		Token    string `json:"token"`
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if !steamIDRe.MatchString(body.SteamID) {
		writeError(w, http.StatusUnprocessableEntity, "steam_id must be 1–20 digits")
		return
	}
	if body.Username != "" {
		if err := session.ValidateUsername(body.Username); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}

	ok, err := steam.ValidateToken(r.Context(), body.SteamID, body.Token)
	if err != nil {
		h.log.Warn("steam token validation failed", zap.String("steam_id", body.SteamID), zap.Error(err))
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid steam token")
		return
	}

	player, err := h.store.UpsertPlayer(r.Context(), body.SteamID, body.Username)
	if err != nil {
		if isUniqueViolation(err) { // username already taken by another account
			writeError(w, http.StatusConflict, "username is already taken")
			return
		}
		h.log.Error("upsert player failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "could not save player")
		return
	}

	ticket := randToken()
	h.tickets.Put(ticket, identity{SteamID: player.SteamID, Tag: player.Tag, Username: player.Username}, wsTicketTTL)
	writeJSON(w, http.StatusOK, map[string]any{
		"tag":      player.Tag,
		"username": player.Username,
		"ticket":   ticket,
		"ttl_ms":   wsTicketTTL.Milliseconds(),
	})
}

// GET /api/v1/leaderboard/hourly?limit=100 — top players for the current UTC hour.
func (h *handler) hourlyLeaderboard(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 100)
	if limit > 200 {
		limit = 200
	}
	if limit < 1 {
		limit = 1
	}
	entries, err := h.store.HourlyLeaderboard(r.Context(), limit)
	if err != nil {
		h.log.Error("leaderboard query failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// GET /ws?ticket=… — upgrade to the game socket. The ticket (single-use, minted
// by /auth) resolves to the player; the SteamID never rides the URL.
func (h *handler) wsConnect(w http.ResponseWriter, r *http.Request) {
	id, ok := h.tickets.Take(r.URL.Query().Get("ticket"))
	if !ok {
		writeError(w, http.StatusUnauthorized, "valid ticket required")
		return
	}
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.log.Warn("ws upgrade failed", zap.Error(err))
		return
	}
	h.hub.ServeClient(ws.NewClient(conn, id.SteamID, id.Tag, id.Username, h.hub))
}

// CORSMiddleware adds Access-Control headers for allowlisted browser origins and
// answers OPTIONS preflight. Non-browser clients are unaffected.
func CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if allowedOrigins[strings.TrimRight(origin, "/")] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- helpers ---

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func queryInt(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
