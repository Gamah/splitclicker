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
	"unicode"

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
	store         *store.Store
	cache         *store.LeaderboardCache
	hub           *ws.Hub
	engine        *game.Engine
	log           *zap.Logger
	tickets       *ticketStore
	upgrader      websocket.Upgrader
	adminPassword string        // ADMIN_PASSWORD; empty disables the /admin surface
	adminSessions *sessionStore // live admin login sessions (cookie tokens)
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
func NewRouter(st *store.Store, cache *store.LeaderboardCache, hub *ws.Hub, engine *game.Engine, log *zap.Logger) *http.ServeMux {
	initOrigins()
	mux := http.NewServeMux()

	// ~1 req/sec sustained, burst 5 — generous for a human, ruinous for a loop.
	rl := newRateLimiter(1, 5)

	h := &handler{
		store:         st,
		cache:         cache,
		hub:           hub,
		engine:        engine,
		log:           log,
		tickets:       newTicketStore(),
		adminPassword: os.Getenv("ADMIN_PASSWORD"),
		adminSessions: newSessionStore(),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin:     checkOrigin,
		},
	}

	mux.HandleFunc("GET /health", h.health)

	// Catch-all for any unmatched path: 404 like the default mux would, but first
	// award the "fart" achievement (poking the backend into a 404 is a feat). The
	// pop rides the socket of any game client open from the same IP. Registered
	// last by pattern specificity — Go 1.22's ServeMux still prefers the routes
	// below this for their exact paths.
	mux.HandleFunc("/", h.notFound)

	// Admin (server-rendered HTML, gated by a login form + session cookie).
	// Disabled unless ADMIN_PASSWORD is set; login POST is rate-limited to blunt
	// password guessing.
	mux.HandleFunc("GET /admin/login", h.adminLoginForm)
	mux.HandleFunc("POST /admin/login", rl.wrap(h.adminLoginSubmit))
	mux.HandleFunc("GET /admin/logout", h.adminLogout)
	mux.HandleFunc("GET /admin", rl.wrap(h.adminDashboard))
	mux.HandleFunc("GET /admin/game", rl.wrap(h.adminGame))
	mux.HandleFunc("GET /admin/media", h.adminMedia)
	mux.HandleFunc("POST /admin/bounties", h.adminBountyCreate)
	mux.HandleFunc("POST /admin/bounties/edit", h.adminBountyEdit)
	mux.HandleFunc("POST /admin/bounties/delete", h.adminBountyDelete)

	// v2 — the real game surface (current client build).
	mux.HandleFunc("GET /api/v2/config", h.config)
	mux.HandleFunc("GET /api/v2/skin", h.skin)
	mux.HandleFunc("POST /api/v2/auth", rl.wrap(h.auth))
	mux.HandleFunc("GET /api/v2/leaderboard/hourly", h.hourlyLeaderboard)
	mux.HandleFunc("GET /api/v2/leaderboard/hours-won", h.hoursWonLeaderboard)
	mux.HandleFunc("GET /api/v2/leaderboard/sessions-won", h.sessionsWonLeaderboard)
	mux.HandleFunc("GET /api/v2/leaderboard/all-time-clicks", h.allTimeClickersLeaderboard)
	mux.HandleFunc("GET /ws/v2", h.wsConnect)

	// v1 — legacy surface for OUTDATED clients still on the old build. Auth/config/
	// skin keep working so the old client connects and looks alive, but the game
	// socket trolls it (joins the loop, ignores its clicks) and every leaderboard
	// returns the "UPDATE UPDATE / 67" board nudging a full s&box restart.
	mux.HandleFunc("GET /api/v1/config", h.config)
	mux.HandleFunc("GET /api/v1/skin", h.skin)
	mux.HandleFunc("POST /api/v1/auth", rl.wrap(h.auth))
	mux.HandleFunc("GET /api/v1/leaderboard/hourly", h.legacyLeaderboard)
	mux.HandleFunc("GET /api/v1/leaderboard/hours-won", h.legacyLeaderboard)
	mux.HandleFunc("GET /api/v1/leaderboard/sessions-won", h.legacyLeaderboard)
	mux.HandleFunc("GET /api/v1/leaderboard/all-time-clicks", h.legacyLeaderboard)
	// Both the bare /ws (what the deployed old client hardcodes) and the explicit
	// /ws/v1 (so a current build set to ApiVersion=v1 can exercise this path) are legacy.
	mux.HandleFunc("GET /ws", h.wsConnectLegacy)
	mux.HandleFunc("GET /ws/v1", h.wsConnectLegacy)

	return mux
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// notFound is the catch-all for any unmatched path: a plain 404, but it first
// tries to award the requester the "fart" achievement. We can only pop it for
// someone who also has a game client open from the same IP (the unlock rides
// their socket); for everyone else the feat is silent.
func (h *handler) notFound(w http.ResponseWriter, r *http.Request) {
	if ip := clientIP(r); h.hub.FireAchievement(ip, "fart") > 0 {
		h.log.Info("fired achievement fart", zap.String("ip", ip))
	}
	http.NotFound(w, r)
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
		SteamID     string `json:"steam_id"`
		Token       string `json:"token"`
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
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
	// The Steam display name is cosmetic (not unique, not a claimable handle), so
	// it isn't validated like a username — just sanitized so junk can't poison the
	// board.
	displayName := sanitizeDisplayName(body.DisplayName)

	ok, err := steam.ValidateToken(r.Context(), body.SteamID, body.Token)
	if err != nil {
		h.log.Warn("steam token validation failed", zap.String("steam_id", body.SteamID), zap.Error(err))
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid steam token")
		return
	}

	player, err := h.store.UpsertPlayer(r.Context(), body.SteamID, body.Username, displayName)
	if err != nil {
		if isUniqueViolation(err) { // username already taken by another account
			writeError(w, http.StatusConflict, "username is already taken")
			return
		}
		h.log.Error("upsert player failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "could not save player")
		return
	}

	// Name() is the claimed username if set, else the Steam display name — the
	// string the client shows for this player everywhere (never the hex tag).
	name := player.Name()
	ticket := randToken()
	h.tickets.Put(ticket, identity{SteamID: player.SteamID, Tag: player.Tag, Username: name}, wsTicketTTL)
	writeJSON(w, http.StatusOK, map[string]any{
		"tag":      player.Tag,
		"username": name,
		"ticket":   ticket,
		"ttl_ms":   wsTicketTTL.Milliseconds(),
	})
}

// All three boards are served from the in-memory LeaderboardCache (refreshed
// per session, not per request), so these handlers never touch Postgres. The
// cache holds at most store.CacheLimit rows; the limit param only narrows that.

// GET /api/v1/leaderboard/hourly?limit=15 — top players for the current UTC hour.
func (h *handler) hourlyLeaderboard(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.cache.Hourly(boardLimit(r)))
}

// GET /api/v1/leaderboard/hours-won?limit=15 — career board of hours won.
func (h *handler) hoursWonLeaderboard(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.cache.HoursWon(boardLimit(r)))
}

// GET /api/v1/leaderboard/sessions-won?limit=15 — games won in the current
// bounty window (the board the bounty winner is read from).
func (h *handler) sessionsWonLeaderboard(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.cache.SessionsWon(boardLimit(r)))
}

// GET /api/v1/leaderboard/all-time-clicks?limit=15 — lifetime top clickers
// (total scoring clicks across all bounties; never resets).
func (h *handler) allTimeClickersLeaderboard(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.cache.AllTimeClickers(boardLimit(r)))
}

// boardLimit reads the ?limit param, defaulting to (and capped at) the cache
// size — there are never more than store.CacheLimit rows to serve.
func boardLimit(r *http.Request) int {
	limit := queryInt(r, "limit", store.CacheLimit)
	if limit > store.CacheLimit {
		limit = store.CacheLimit
	}
	if limit < 1 {
		limit = 1
	}
	return limit
}

// GET /ws/v2?ticket=… — upgrade to the game socket. The ticket (single-use, minted
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
	h.hub.ServeClient(ws.NewClient(conn, id.SteamID, id.Tag, id.Username, clientIP(r), h.hub))
}

// GET /ws — the legacy game socket for OUTDATED clients (the new build uses
// /ws/v2). It joins them to the normal broadcast loop so their UI behaves, but the
// hub ignores their clicks and feeds them the troll leaderboard; they're excluded
// from the live player count. The cure is to fully restart s&box for the new build.
func (h *handler) wsConnectLegacy(w http.ResponseWriter, r *http.Request) {
	id, ok := h.tickets.Take(r.URL.Query().Get("ticket"))
	if !ok {
		writeError(w, http.StatusUnauthorized, "valid ticket required")
		return
	}
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.log.Warn("legacy ws upgrade failed", zap.Error(err))
		return
	}
	c := ws.NewClient(conn, id.SteamID, id.Tag, id.Username, clientIP(r), h.hub)
	c.Legacy = true
	h.hub.ServeClient(c)
}

// legacyLeaderboard serves the troll board (15× "UPDATE UPDATE" / 67) to every v1
// leaderboard request, whichever board or limit was asked for — the visible nudge
// for outdated clients to restart s&box.
func (h *handler) legacyLeaderboard(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ws.LegacyBoard())
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

// sanitizeDisplayName trims a client-reported Steam name, strips control
// characters, and caps the length. It is cosmetic only — uniqueness/charset
// rules apply to claimed usernames, not to this.
func sanitizeDisplayName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	const maxRunes = 32
	if r := []rune(s); len(r) > maxRunes {
		s = string(r[:maxRunes])
	}
	return s
}

func queryInt(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
