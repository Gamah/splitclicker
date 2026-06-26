package api

import (
	"crypto/subtle"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gamah/splitclicker/internal/game"
	"github.com/gamah/splitclicker/internal/store"
	"github.com/gamah/splitclicker/internal/ws"
	"go.uber.org/zap"
)

// adminSessionTTL bounds how long an admin login lasts before re-auth is needed.
const adminSessionTTL = 12 * time.Hour

// adminCookieName is the session cookie set after a successful login.
const adminCookieName = "admin_session"

// adminPageSize is the default rows-per-page for every paginated admin table.
const adminPageSize = 20

// sessionStore is the set of live admin session tokens (token → expiry). It is
// in-memory, so a server restart logs admins out — fine for this tiny surface.
// Each write opportunistically sweeps expired tokens (volume here is trivial).
type sessionStore struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func newSessionStore() *sessionStore { return &sessionStore{m: map[string]time.Time{}} }

// create mints a new session token valid for adminSessionTTL.
func (s *sessionStore) create() string {
	tok := randToken()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, exp := range s.m {
		if now.After(exp) {
			delete(s.m, k)
		}
	}
	s.m[tok] = now.Add(adminSessionTTL)
	return tok
}

// valid reports whether tok is a live (unexpired) session.
func (s *sessionStore) valid(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.m, tok)
		return false
	}
	return true
}

func (s *sessionStore) delete(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, tok)
}

// adminEnabled reports whether the admin surface is configured. When
// ADMIN_PASSWORD is unset the whole surface is disabled (404) so a misconfigured
// deploy can never expose an open admin — and 404 (not 403) so it doesn't even
// reveal the surface exists.
func (h *handler) adminEnabled(w http.ResponseWriter, r *http.Request) bool {
	if h.adminPassword == "" {
		http.NotFound(w, r)
		return false
	}
	return true
}

// adminAuth gates the admin views: enabled + a valid session cookie. An
// unauthenticated request is redirected to the login page. Returns true when the
// request may proceed.
func (h *handler) adminAuth(w http.ResponseWriter, r *http.Request) bool {
	if !h.adminEnabled(w, r) {
		return false
	}
	if h.adminSessions.valid(sessionCookie(r)) {
		return true
	}
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
	return false
}

// GET /admin/login — the password form (a single password field + Login button).
// Already-authenticated requests skip straight to the dashboard.
func (h *handler) adminLoginForm(w http.ResponseWriter, r *http.Request) {
	if !h.adminEnabled(w, r) {
		return
	}
	if h.adminSessions.valid(sessionCookie(r)) {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	h.renderAdmin(w, loginTmpl, loginData{})
}

// POST /admin/login — verify the password (constant-time), mint a session, set
// the cookie, and redirect to the dashboard. Re-renders the form on failure.
func (h *handler) adminLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if !h.adminEnabled(w, r) {
		return
	}
	pass := r.PostFormValue("password")
	if subtle.ConstantTimeCompare([]byte(pass), []byte(h.adminPassword)) != 1 {
		// Fumbling the admin password earns the "hackerman" achievement — popped on
		// any game client open from the same IP (silent if there's none).
		if ip := clientIP(r); h.hub.FireAchievement(ip, "hackerman") > 0 {
			h.log.Info("fired achievement hackerman", zap.String("ip", ip))
		}
		h.renderAdmin(w, loginTmpl, loginData{Error: "Incorrect password."})
		return
	}
	setAdminCookie(w, r, h.adminSessions.create())
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// GET /admin/logout — drop the session and clear the cookie.
func (h *handler) adminLogout(w http.ResponseWriter, r *http.Request) {
	if !h.adminEnabled(w, r) {
		return
	}
	if c, err := r.Cookie(adminCookieName); err == nil {
		h.adminSessions.delete(c.Value)
	}
	clearAdminCookie(w, r)
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

// sessionCookie reads the admin session token from the request ("" if absent).
func sessionCookie(r *http.Request) string {
	if c, err := r.Cookie(adminCookieName); err == nil {
		return c.Value
	}
	return ""
}

func setAdminCookie(w http.ResponseWriter, r *http.Request, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    tok,
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		MaxAge:   int(adminSessionTTL / time.Second),
	})
}

func clearAdminCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    "",
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		MaxAge:   -1,
	})
}

// requestIsHTTPS marks the cookie Secure when the original request was TLS —
// either direct or via Caddy's X-Forwarded-Proto (it terminates TLS and proxies
// plain HTTP to us). Plain-HTTP local dev stays non-Secure so the cookie works.
func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// GET /admin — dashboard: history counts, the live leaderboards, the recent
// games, and the most-recent anticheat checks. Every table is paginated
// (adminPageSize rows/page, each with its own page query param) and the whole
// view can be scoped to a single bounty window via the ?bounty= filter.
func (h *handler) adminDashboard(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuth(w, r) {
		return
	}
	ctx := r.Context()

	selectable, err := h.store.SelectableBounties(ctx)
	if err != nil {
		h.adminError(w, "load bounty filter", err)
		return
	}
	win, filterLabel, bounty := resolveWindow(r.URL.Query().Get("bounty"), selectable)

	stats, err := h.store.AdminStats(ctx, win)
	if err != nil {
		h.adminError(w, "load stats", err)
		return
	}

	gNum, gOff := pageOffset(r, "gp")
	games, gTotal, err := h.store.RecentGames(ctx, win, adminPageSize, gOff)
	if err != nil {
		h.adminError(w, "load recent games", err)
		return
	}
	acNum, acOff := pageOffset(r, "acp")
	antiCheat, acTotal, err := h.store.RecentChecks(ctx, win, adminPageSize, acOff)
	if err != nil {
		h.adminError(w, "load recent checks", err)
		return
	}
	fNum, fOff := pageOffset(r, "fp")
	fastest, fTotal, err := h.store.FastestClickers(ctx, win, adminPageSize, fOff)
	if err != nil {
		h.adminError(w, "load fastest clickers", err)
		return
	}
	pNum, pOff := pageOffset(r, "pp")
	points, pTotal, err := h.store.PointsBoard(ctx, win, adminPageSize, pOff)
	if err != nil {
		h.adminError(w, "load points board", err)
		return
	}
	hNum, hOff := pageOffset(r, "hp")
	hoursWon, hTotal, err := h.store.HoursWonBoard(ctx, win, adminPageSize, hOff)
	if err != nil {
		h.adminError(w, "load hours-won board", err)
		return
	}
	sNum, sOff := pageOffset(r, "sp")
	sessionsWon, sTotal, err := h.store.SessionsWonBoard(ctx, win, adminPageSize, sOff)
	if err != nil {
		h.adminError(w, "load games-won board", err)
		return
	}
	aNum, aOff := pageOffset(r, "ap")
	allTime, aTotal, err := h.store.AllTimeClickersBoard(ctx, win, adminPageSize, aOff)
	if err != nil {
		h.adminError(w, "load all-time board", err)
		return
	}

	// The bounty queue itself is the management surface, not a filtered view, so
	// it always shows every bounty regardless of the active filter.
	bounties, err := h.store.ListBounties(ctx)
	if err != nil {
		h.adminError(w, "load bounties", err)
		return
	}

	// Live connections (the drop panel) + the silent-ban list — both are live
	// moderation state, independent of the bounty window filter.
	shadowbans, err := h.store.ListShadowbans(ctx)
	if err != nil {
		h.adminError(w, "load shadowbans", err)
		return
	}

	mk := func(param string, num, total int) Page {
		return Page{Base: "/admin", Bounty: bounty, Param: param, Num: num, Size: adminPageSize, Total: total}
	}
	data := dashboardData{
		Stats:           stats,
		Players:         h.hub.PlayerCount(),
		Bounty:          bounty,
		FilterLabel:     filterLabel,
		Selectable:      selectable,
		Bounties:        bounties,
		Connections:     h.hub.Connections(),
		Shadowbans:      shadowbans,
		Games:           games,
		GamesPage:       mk("gp", gNum, gTotal),
		AntiCheat:       antiCheat,
		AntiCheatPage:   mk("acp", acNum, acTotal),
		Fastest:         fastest,
		FastestPage:     mk("fp", fNum, fTotal),
		Points:          points,
		PointsPage:      mk("pp", pNum, pTotal),
		HoursWon:        hoursWon,
		HoursWonPage:    mk("hp", hNum, hTotal),
		SessionsWon:     sessionsWon,
		SessionsWonPage: mk("sp", sNum, sTotal),
		AllTime:         allTime,
		AllTimePage:     mk("ap", aNum, aTotal),
	}
	h.renderAdmin(w, dashboardTmpl, data)
}

// GET /admin/player?id=<steamid>&bounty=… — the per-player profile: identity (a
// link out to their Steam profile), window-scoped aggregate stats, and the
// paginated detail of their anticheat checks and tests.
func (h *handler) adminPlayer(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuth(w, r) {
		return
	}
	id := r.URL.Query().Get("id")
	if !steamIDRe.MatchString(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	selectable, err := h.store.SelectableBounties(ctx)
	if err != nil {
		h.adminError(w, "load bounty filter", err)
		return
	}
	win, filterLabel, bounty := resolveWindow(r.URL.Query().Get("bounty"), selectable)

	profile, ok, err := h.store.PlayerProfile(ctx, id, win)
	if err != nil {
		h.adminError(w, "load player", err)
		return
	}
	if !ok {
		http.Error(w, "player not found", http.StatusNotFound)
		return
	}

	gNum, gOff := pageOffset(r, "gp")
	games, gTotal, err := h.store.PlayerGames(ctx, id, win, adminPageSize, gOff)
	if err != nil {
		h.adminError(w, "load player games", err)
		return
	}
	cNum, cOff := pageOffset(r, "cp")
	checks, cTotal, err := h.store.PlayerChecks(ctx, id, win, adminPageSize, cOff)
	if err != nil {
		h.adminError(w, "load player checks", err)
		return
	}
	tNum, tOff := pageOffset(r, "tp")
	tests, tTotal, err := h.store.PlayerTests(ctx, id, win, adminPageSize, tOff)
	if err != nil {
		h.adminError(w, "load player tests", err)
		return
	}

	// Live anticheat ladder state for the active bounty (independent of the bounty
	// filter above — the ladder only exists for the bounty currently in play).
	sanction, err := h.store.PlayerSanction(ctx, id)
	if err != nil {
		h.adminError(w, "load player sanction", err)
		return
	}

	banned, err := h.store.IsShadowbanned(ctx, id)
	if err != nil {
		h.adminError(w, "load shadowban state", err)
		return
	}

	extra := "id=" + url.QueryEscape(id)
	mk := func(param string, num, total int) Page {
		return Page{Base: "/admin/player", Extra: extra, Bounty: bounty, Param: param, Num: num, Size: adminPageSize, Total: total}
	}
	data := playerData{
		Profile:     profile,
		Selectable:  selectable,
		Bounty:      bounty,
		FilterLabel:  filterLabel,
		Sanction:     sanction,
		Shadowbanned: banned,
		Games:        games,
		GamesPage:    mk("gp", gNum, gTotal),
		Checks:       checks,
		ChecksPage: mk("cp", cNum, cTotal),
		Tests:      tests,
		TestsPage:  mk("tp", tNum, tTotal),
	}
	h.renderAdmin(w, playerTmpl, data)
}

// POST /admin/player/checks — set a player's anticheat flag count for the active
// bounty (clearing any cooldown/ignored). Writes the store row and pushes the new
// state to the engine so it applies live, then returns to the player page.
func (h *handler) adminPlayerChecks(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuth(w, r) {
		return
	}
	id := r.FormValue("id")
	if !steamIDRe.MatchString(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	checks, err := strconv.Atoi(strings.TrimSpace(r.FormValue("checks")))
	if err != nil || checks < 0 {
		http.Error(w, "checks must be a non-negative integer", http.StatusBadRequest)
		return
	}

	bountyID, _, ok := h.cache.ActiveBountyMeta()
	if !ok || bountyID == 0 {
		http.Error(w, "no active bounty — nothing to sanction", http.StatusConflict)
		return
	}
	// Derive the rung the count lands on (>= threshold → cooldown, more → ignored) so
	// the edit takes effect now, not on the next real flag. Persist that full state
	// synchronously (so the reloaded page reflects it), then push it to the engine to
	// apply live (block scoring + notify the client).
	s := game.Sanction{SteamID: id, Checks: checks}
	if h.engine != nil {
		s = h.engine.SanctionForChecks(checks)
		s.SteamID = id
	}
	if err := h.store.SaveSanction(r.Context(), bountyID, s); err != nil {
		h.adminError(w, "set player checks", err)
		return
	}
	if h.engine != nil {
		h.engine.SetSanction(id, s)
	}
	http.Redirect(w, r, "/admin/player?id="+url.QueryEscape(id), http.StatusSeeOther)
}

// POST /admin/shadowban — add or remove a SteamID from the silent-ban list.
// action=ban adds (with an optional reason); action=unban removes. Enforcement is
// at the WS connect path, so a ban takes effect on the player's NEXT connect — use
// the connections panel's "drop" to force a reconnect now. `back` chooses the
// redirect target so the form works from both the dashboard and a player page.
func (h *handler) adminShadowban(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuth(w, r) {
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	if !steamIDRe.MatchString(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var err error
	switch r.FormValue("action") {
	case "ban":
		err = h.store.AddShadowban(r.Context(), id, strings.TrimSpace(r.FormValue("reason")))
	case "unban":
		err = h.store.RemoveShadowban(r.Context(), id)
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	if err != nil {
		h.adminError(w, "update shadowban", err)
		return
	}
	if r.FormValue("back") == "player" {
		http.Redirect(w, r, "/admin/player?id="+url.QueryEscape(id), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// POST /admin/connections/drop — force-close a live connection so the client has
// to reconnect. Paired with a saved shadowban, this is how a ban is made to take
// effect immediately (the reconnect re-runs the connect-time ban check).
func (h *handler) adminDropConnection(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuth(w, r) {
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	if !steamIDRe.MatchString(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	h.hub.Drop(id) // no-op if that account has no socket open
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// GET /admin/game?id=… — one game's rounds and the per-click arrival timing.
func (h *handler) adminGame(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuth(w, r) {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	detail, ok, err := h.store.GameDetail(r.Context(), id)
	if err != nil {
		h.adminError(w, "load game", err)
		return
	}
	if !ok {
		http.Error(w, "game not found", http.StatusNotFound)
		return
	}
	h.renderAdmin(w, gameTmpl, detail)
}

func (h *handler) adminError(w http.ResponseWriter, what string, err error) {
	h.log.Error("admin: "+what, zap.Error(err))
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func (h *handler) renderAdmin(w http.ResponseWriter, t *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, data); err != nil {
		h.log.Error("admin: render", zap.Error(err))
	}
}

// pageOffset reads a 1-based page number from the named query param (default 1,
// floored at 1) and returns it together with the row offset it implies.
func pageOffset(r *http.Request, param string) (num, offset int) {
	num = queryInt(r, param, 1)
	if num < 1 {
		num = 1
	}
	return num, (num - 1) * adminPageSize
}

// resolveWindow turns the ?bounty= filter value into a store.Window. "", "all",
// or an unknown id all mean the default all-history view; a known bounty id
// scopes every view to that bounty's window. It returns the window, a label for
// the page heading, and the normalised filter value to echo back into links
// ("" for all-history).
func resolveWindow(param string, selectable []store.BountyWindow) (store.Window, string, string) {
	if param != "" && param != "all" {
		for _, b := range selectable {
			if strconv.FormatInt(b.ID, 10) == param {
				return b.Window(), bountyFilterLabel(b), param
			}
		}
	}
	return store.AllWindow(), "All history", ""
}

// bwName is a bounty's display name for the filter (its label, or "#id").
func bwName(b store.BountyWindow) string {
	if b.Label != "" {
		return b.Label
	}
	return "bounty #" + strconv.FormatInt(b.ID, 10)
}

// bountyFilterLabel is the human label for a bounty in the filter / heading.
func bountyFilterLabel(b store.BountyWindow) string {
	name := b.Label
	if name == "" {
		name = "bounty #" + strconv.FormatInt(b.ID, 10)
	}
	if b.Status == "active" {
		return name + " (current)"
	}
	return name
}

// Page is the pagination state for one admin table. It carries everything its
// pager control needs to build prev/next links that keep the page's base path,
// any fixed query (Extra, e.g. the player id) and the active bounty filter,
// while changing only this table's page param. Switching one table's page resets
// the other tables on the page to page 1 (their params aren't carried) — fine
// for an admin surface and keeps link building simple.
type Page struct {
	Base   string // "/admin" or "/admin/player"
	Extra  string // pre-encoded query to preserve (e.g. "id=765…"), or ""
	Bounty string // active bounty filter ("" = all history)
	Param  string // the query param controlling this table's page
	Num    int
	Size   int
	Total  int
}

// Last is the highest page number (at least 1).
func (p Page) Last() int {
	if p.Size <= 0 || p.Total <= 0 {
		return 1
	}
	return (p.Total + p.Size - 1) / p.Size
}

func (p Page) HasPrev() bool { return p.Num > 1 }
func (p Page) HasNext() bool { return p.Num < p.Last() }

// From / To are the 1-based row range shown ("From–To of Total").
func (p Page) From() int {
	if p.Total == 0 {
		return 0
	}
	return (p.Num-1)*p.Size + 1
}

func (p Page) To() int {
	to := p.Num * p.Size
	if to > p.Total {
		to = p.Total
	}
	return to
}

func (p Page) link(n int) string {
	v := url.Values{}
	if p.Bounty != "" {
		v.Set("bounty", p.Bounty)
	}
	v.Set(p.Param, strconv.Itoa(n))
	q := v.Encode()
	if p.Extra != "" {
		q = p.Extra + "&" + q
	}
	return p.Base + "?" + q
}

func (p Page) PrevURL() string { return p.link(p.Num - 1) }
func (p Page) NextURL() string { return p.link(p.Num + 1) }

type loginData struct{ Error string }

type dashboardData struct {
	Stats       store.AdminStats
	Players     int
	Bounty      string // active filter value ("" = all history)
	FilterLabel string
	Selectable  []store.BountyWindow
	Bounties    []store.Bounty

	Connections []ws.ConnInfo     // live sockets (the drop panel)
	Shadowbans  []store.Shadowban // current silent-ban list

	Games     []store.AdminGame
	GamesPage Page

	AntiCheat     []store.AdminCheck
	AntiCheatPage Page

	Fastest     []store.FastestClicker
	FastestPage Page

	Points     []store.LeaderboardEntry
	PointsPage Page

	HoursWon     []store.LeaderboardEntry
	HoursWonPage Page

	SessionsWon     []store.LeaderboardEntry
	SessionsWonPage Page

	AllTime     []store.LeaderboardEntry
	AllTimePage Page
}

type playerData struct {
	Profile     store.PlayerProfile
	Selectable  []store.BountyWindow
	Bounty      string
	FilterLabel string

	Sanction     store.PlayerSanction // live ladder state for the active bounty (editable)
	Shadowbanned bool                 // on the silent-ban list

	Games     []store.PlayerGame
	GamesPage Page

	Checks     []store.AdminCheck
	ChecksPage Page

	Tests     []store.AdminTest
	TestsPage Page
}

// localTime renders an instant as a <time> element carrying the UTC instant in
// its datetime attribute, with a UTC fallback as text. The shared admin JS
// rewrites the text to the browser's local zone on load; with JS off the UTC
// value still shows. Storage and the wire stay UTC — this is display only.
func localTime(t time.Time) template.HTML {
	return template.HTML(`<time class="lt" datetime="` + t.UTC().Format(time.RFC3339) +
		`">` + t.UTC().Format("2006-01-02 15:04:05") + ` UTC</time>`)
}

// adminFuncs are the template helpers shared by the admin views.
var adminFuncs = template.FuncMap{
	// lt / ltp render a timestamp for local-time display (see localTime); ltp shows
	// "—" for an unset nullable time (e.g. a pending bounty's activation).
	"lt": func(t time.Time) template.HTML { return localTime(t) },
	"ltp": func(t *time.Time) template.HTML {
		if t == nil {
			return template.HTML("—")
		}
		return localTime(*t)
	},
	// rfc is the UTC RFC3339 instant the admin JS parses to relabel a filter option
	// in local time (an <option> can't hold a <time> element, so it's data-driven).
	"rfc":    func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"bwname": bwName,
	// dtlocal formats a time as an <input type=datetime-local> value (UTC, no
	// zone) so editing a bounty round-trips through parseAdminTime's UTC reading.
	"dtlocal": func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04") },
	"dur": func(a, b time.Time) string {
		d := b.Sub(a).Round(time.Second)
		if d < 0 {
			d = 0
		}
		return d.String()
	},
	"short": func(s string) string { // first segment of a UUID, enough to eyeball
		if len(s) >= 8 {
			return s[:8]
		}
		return s
	},
	"add": func(a, b int) int { return a + b },
	// dict builds a map from alternating key/value args, so a shared template
	// block (the filter bar) can be invoked with a small ad-hoc payload.
	"dict": func(pairs ...any) map[string]any {
		m := make(map[string]any, len(pairs)/2)
		for i := 0; i+1 < len(pairs); i += 2 {
			if k, ok := pairs[i].(string); ok {
				m[k] = pairs[i+1]
			}
		}
		return m
	},
	// idsel reports whether bounty option id matches the active filter value, for
	// the <option selected> marker.
	"idsel": func(id int64, cur string) bool { return strconv.FormatInt(id, 10) == cur },
	// bwlabel is the no-JS fallback option text for a bounty in the filter dropdown:
	// its name plus its window in UTC. The admin JS relabels it in local time from
	// the option's data-* attributes (rfc/bwname feed those).
	"bwlabel": func(b store.BountyWindow) string {
		when := b.Start.UTC().Format("2006-01-02 15:04")
		if b.Status == "active" {
			return bwName(b) + "  (current · since " + when + " UTC)"
		}
		return bwName(b) + "  (" + when + " → " + b.End.UTC().Format("2006-01-02 15:04") + " UTC)"
	},
	// steamurl is the public Steam community profile URL for a SteamID64.
	"steamurl": func(steamID string) string {
		return "https://steamcommunity.com/profiles/" + steamID
	},
	// plink renders a player name as a link to their per-player admin profile
	// (which in turn links out to Steam). The name (a user-controlled display
	// string) is HTML-escaped; an empty steam id or name degrades gracefully.
	// Returns template.HTML so it isn't re-escaped.
	"plink": func(steamID, name string) template.HTML {
		if name == "" {
			name = "anon"
		}
		safeName := template.HTMLEscapeString(name)
		if steamID == "" {
			return template.HTML(safeName)
		}
		href := "/admin/player?id=" + template.HTMLEscapeString(steamID)
		return template.HTML(`<a href="` + href + `">` + safeName + `</a>`)
	},
}

const adminCSS = `
:root{
  --bg:#0f1115;--panel:#171a21;--panel2:#1e222b;--line:#2a2f3a;
  --text:#e7eaf0;--muted:#9aa3b2;--accent:#5b9dff;--accent2:#7ee0a8;
  --warn:#ff6b6b;--ok:#42d392;--shadow:0 1px 2px rgba(0,0,0,.4),0 8px 24px rgba(0,0,0,.25);
}
*{box-sizing:border-box}
body{font:14px/1.55 system-ui,-apple-system,Segoe UI,Roboto,sans-serif;margin:0;
  color:var(--text);background:radial-gradient(1200px 600px at 80% -10%,#1b2030,#0f1115);min-height:100vh}
.wrap{max-width:1180px;margin:0 auto;padding:1.5rem 1.5rem 4rem}
h1{font-size:1.5rem;font-weight:700;letter-spacing:-.01em;margin:.2rem 0}
h2{font-size:1.05rem;font-weight:650;margin:2rem 0 .25rem}
h2 .muted{font-weight:400;font-size:.8rem}
h3{margin:0 .5rem 0 0;font-weight:600;font-size:1rem}
a{color:var(--accent);text-decoration:none}a:hover{text-decoration:underline}
.topbar{display:flex;align-items:center;justify-content:space-between;gap:1rem;
  padding-bottom:1rem;border-bottom:1px solid var(--line);margin-bottom:1.25rem}
.brand{display:flex;align-items:baseline;gap:.6rem}
.brand .badge{font-size:.7rem;color:var(--accent2);border:1px solid var(--line);
  border-radius:999px;padding:.1rem .55rem;background:var(--panel)}
.logout{font-size:.85rem;color:var(--muted)}
.cards{display:flex;gap:.75rem;flex-wrap:wrap;margin:.5rem 0}
.card{background:linear-gradient(180deg,var(--panel2),var(--panel));border:1px solid var(--line);
  border-radius:12px;padding:.7rem 1.1rem;min-width:7rem;box-shadow:var(--shadow)}
.card .n{font-size:1.7rem;font-weight:750;letter-spacing:-.02em;line-height:1.1}
.card .lbl{color:var(--muted);font-size:.8rem}
table{border-collapse:separate;border-spacing:0;width:100%;margin-top:.5rem;
  background:var(--panel);border:1px solid var(--line);border-radius:12px;overflow:hidden;box-shadow:var(--shadow)}
th,td{padding:.5rem .7rem;text-align:left;border-bottom:1px solid var(--line)}
th{background:var(--panel2);color:var(--muted);font-weight:600;font-size:.78rem;
  text-transform:uppercase;letter-spacing:.04em}
tbody tr:last-child td{border-bottom:none}
tbody tr:hover{background:rgba(91,157,255,.06)}
td.num,th.num{text-align:right;font-variant-numeric:tabular-nums}
.cols{display:flex;gap:1rem;flex-wrap:wrap}.cols>div{flex:1;min-width:16rem}
.cols h2{margin-top:1rem;font-size:.95rem}
.muted{color:var(--muted)}.mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.92em}
.err{color:var(--warn);margin:.5rem 0}.ok{color:var(--ok)}
.flag{color:var(--warn);font-weight:600}
.pill{display:inline-block;font-size:.72rem;padding:.08rem .5rem;border-radius:999px;border:1px solid var(--line)}
.pill.active{color:var(--accent2);border-color:#2c5a44;background:rgba(126,224,168,.08)}
.pill.pending{color:#ffd479}
.pill.won{color:var(--muted)}
.pill.archived{color:#ffb37a;border-color:#5a4632;background:rgba(255,179,122,.08)}
.filterbar{display:flex;align-items:center;gap:.6rem;flex-wrap:wrap;margin:.25rem 0 1rem;
  background:var(--panel);border:1px solid var(--line);border-radius:12px;padding:.6rem .9rem}
.filterbar label{color:var(--muted);font-size:.85rem}
.filterbar select{background:var(--panel2);color:var(--text);border:1px solid var(--line);
  border-radius:8px;padding:.35rem .6rem;font-size:.9rem;min-width:18rem}
.filterbar .scope{margin-left:auto;color:var(--muted);font-size:.85rem}
.pager{display:flex;align-items:center;gap:.75rem;margin:.5rem 0 .25rem;font-size:.85rem}
.pager a{padding:.2rem .6rem;border:1px solid var(--line);border-radius:8px;background:var(--panel)}
.pager .disabled{padding:.2rem .6rem;border:1px solid var(--line);border-radius:8px;
  color:var(--muted);opacity:.5}
.pager .range{color:var(--muted)}
.login{max-width:21rem;margin:7rem auto;background:var(--panel);border:1px solid var(--line);
  border-radius:14px;padding:1.6rem;box-shadow:var(--shadow)}
.login h1{margin-bottom:.6rem}
.login input{width:100%;padding:.6rem;margin:.4rem 0;box-sizing:border-box;font-size:1rem;
  background:var(--panel2);border:1px solid var(--line);border-radius:8px;color:var(--text)}
.login button{width:100%;padding:.6rem;font-size:1rem;cursor:pointer;border:none;border-radius:8px;
  background:var(--accent);color:#06122b;font-weight:600}
img.skin{height:40px;border-radius:6px;display:block;background:#0b0d12}
input.inspect{min-width:18rem}
.addbounty input.inspect{min-width:22rem}
/* the per-bounty inspect link is locked (readonly) until its "edit" button is
   clicked, so it can't be changed by accident on an unrelated save. */
input.inspect[readonly]{opacity:.55;cursor:not-allowed}
.inspect-edit{display:inline-flex;gap:.3rem;align-items:center}
button.editbtn{font-size:.8rem;cursor:pointer;background:var(--panel2);color:var(--text);
  border:1px solid var(--line);border-radius:6px;padding:.25rem .5rem}
td input,td button,td select{font-size:.85rem;background:var(--panel2);color:var(--text);
  border:1px solid var(--line);border-radius:6px;padding:.25rem .4rem}
td button{cursor:pointer}
.addbounty{margin-top:1rem;padding:1rem;background:var(--panel);border:1px solid var(--line);
  border-radius:12px;display:flex;gap:.6rem;align-items:center;flex-wrap:wrap}
.addbounty input{background:var(--panel2);color:var(--text);border:1px solid var(--line);
  border-radius:8px;padding:.4rem .55rem}
.addbounty button{cursor:pointer;border:none;border-radius:8px;background:var(--accent);
  color:#06122b;font-weight:600;padding:.45rem .9rem}
.actions{display:flex;gap:.4rem}
.backlink{display:inline-block;margin-bottom:.5rem;color:var(--muted)}
`

// adminJS is shared by every admin view that shows timestamps. It rewrites the
// UTC <time> elements (and the filter dropdown's window labels) into the browser's
// local zone, and round-trips datetime-local inputs through local⇄UTC so the host
// edits in local time while the wire + backend stay UTC. No backticks (this is a
// Go raw string); single-quoted JS only.
const adminJS = `
<script>
(function(){
  function pad(n){return (n<10?'0':'')+n;}
  function fmtLocal(d){return d.getFullYear()+'-'+pad(d.getMonth()+1)+'-'+pad(d.getDate())+'T'+pad(d.getHours())+':'+pad(d.getMinutes());}
  function fmtUTC(d){return d.getUTCFullYear()+'-'+pad(d.getUTCMonth()+1)+'-'+pad(d.getUTCDate())+'T'+pad(d.getUTCHours())+':'+pad(d.getUTCMinutes());}
  // Display timestamps: <time class="lt" datetime="<RFC3339 UTC>">UTC fallback</time>
  // → the browser's local zone.
  document.querySelectorAll('time.lt').forEach(function(el){
    var d=new Date(el.getAttribute('datetime'));
    if(!isNaN(d.getTime())) el.textContent=d.toLocaleString();
  });
  // Filter <option>s can't hold a <time>, so they carry data-* and we relabel the
  // window in local time here.
  document.querySelectorAll('option[data-start]').forEach(function(el){
    var s=new Date(el.getAttribute('data-start'));
    if(isNaN(s.getTime())) return;
    var name=el.getAttribute('data-name')||'';
    if(el.getAttribute('data-status')==='active'){
      el.textContent=name+'  (current · since '+s.toLocaleString()+')';
    }else{
      var e=new Date(el.getAttribute('data-end'));
      el.textContent=name+'  ('+s.toLocaleString()+' → '+(isNaN(e.getTime())?'?':e.toLocaleString())+')';
    }
  });
  // datetime-local inputs: server value is the UTC wall-clock (also in data-utc).
  // Show it in local time; convert back to UTC right before submit so parseAdminTime
  // (which reads the field as UTC) still gets UTC.
  document.querySelectorAll('input[type=datetime-local][data-utc]').forEach(function(el){
    var d=new Date(el.getAttribute('data-utc')+'Z');
    if(!isNaN(d.getTime())) el.value=fmtLocal(d);
  });
  document.querySelectorAll('form').forEach(function(f){
    f.addEventListener('submit',function(){
      Array.prototype.forEach.call(f.elements,function(el){
        if(el.type==='datetime-local' && el.value){
          var d=new Date(el.value); // no zone ⇒ parsed as local wall-clock
          if(!isNaN(d.getTime())) el.value=fmtUTC(d);
        }
      });
    });
  });
})();
</script>
`

// loginTmpl is the password gate.
var loginTmpl = template.Must(template.New("login").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>splitclicker admin · login</title><style>` + adminCSS + `</style></head><body>
<form class="login" method="post" action="/admin/login">
  <h1>admin</h1>
  {{if .Error}}<p class="err">{{.Error}}</p>{{end}}
  <input type="password" name="password" placeholder="password" autofocus autocomplete="current-password">
  <button type="submit">Login</button>
</form>
</body></html>`))

// pagerTmpl is the shared prev/next control, invoked as {{template "pager" .SomePage}}.
const pagerTmpl = `
{{define "pager"}}{{if gt .Total 0}}
<div class="pager">
  <span class="range">{{.From}}–{{.To}} of {{.Total}}</span>
  {{if .HasPrev}}<a href="{{.PrevURL}}">‹ prev</a>{{else}}<span class="disabled">‹ prev</span>{{end}}
  {{if .HasNext}}<a href="{{.NextURL}}">next ›</a>{{else}}<span class="disabled">next ›</span>{{end}}
</div>
{{end}}{{end}}`

// filterTmpl is the shared "filter by bounty" bar. The caller defines a
// "filterform" block first (it differs between the dashboard and player views,
// which post different hidden fields).
const filterTmpl = `
{{define "filter"}}
<form class="filterbar" method="get" action="{{.Action}}">
  {{template "filterhidden" .}}
  <label for="bounty">Filter by bounty</label>
  <select id="bounty" name="bounty" onchange="this.form.submit()">
    <option value="" {{if eq .Bounty ""}}selected{{end}}>All history</option>
    {{range .Selectable}}<option value="{{.ID}}" {{if idsel .ID $.Bounty}}selected{{end}} data-start="{{rfc .Start}}" data-end="{{rfc .End}}" data-name="{{bwname .}}" data-status="{{.Status}}">{{bwlabel .}}</option>{{end}}
  </select>
  <noscript><button type="submit">Apply</button></noscript>
  <span class="scope">showing: {{.FilterLabel}}</span>
</form>
{{end}}`

var dashboardTmpl = template.Must(template.New("dash").Funcs(adminFuncs).Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>splitclicker admin</title><style>` + adminCSS + `</style></head><body><div class="wrap">
<div class="topbar">
  <div class="brand"><h1>splitclicker admin</h1><span class="badge">{{.Players}} online</span></div>
  <a class="logout" href="/admin/logout">log out</a>
</div>

` + pagerTmpl + filterTmpl + `
{{define "filterhidden"}}{{end}}
{{template "filter" (dict "Action" "/admin" "Bounty" .Bounty "FilterLabel" .FilterLabel "Selectable" .Selectable)}}

<div class="cards">
  <div class="card"><div class="n">{{.Stats.Games}}</div><div class="lbl">games</div></div>
  <div class="card"><div class="n">{{.Stats.Rounds}}</div><div class="lbl">rounds</div></div>
  <div class="card"><div class="n">{{.Stats.Clicks}}</div><div class="lbl">scoring clicks</div></div>
  <div class="card"><div class="n">{{.Stats.Checks}}</div><div class="lbl">anticheat checks</div></div>
  <div class="card"><div class="n">{{.Stats.Tests}}</div><div class="lbl">tests sent</div></div>
  <div class="card"><div class="n">{{.Stats.Players}}</div><div class="lbl">accounts (all-time)</div></div>
</div>

<h2>Connections <span class="muted">· live sockets ({{len .Connections}}). "drop" force-closes a socket so the client reconnects — paired with a shadowban, that's how a ban takes effect now (the reconnect re-runs the ban check). "shadowban" here adds the account to the silent-ban list; it then takes effect on its next connect, so drop it too.</span></h2>
<table>
  <tr><th>player</th><th>tag</th><th class="num">ver</th><th>flags</th><th>ip</th><th></th></tr>
  {{range .Connections}}
  <tr>
    <td>{{plink .SteamID .Username}}</td>
    <td class="mono">{{.Tag}}</td>
    <td class="num">{{.Version}}</td>
    <td>
      {{if .Shadowbanned}}<span class="pill archived">shadowbanned</span>{{end}}
      {{if .Legacy}}<span class="pill">legacy</span>{{end}}
      {{if .Parked}}<span class="pill">parked</span>{{end}}
    </td>
    <td class="mono">{{.IP}}</td>
    <td>
      <div class="actions">
        {{if .Shadowbanned}}
        <form method="post" action="/admin/shadowban"><input type="hidden" name="id" value="{{.SteamID}}"><input type="hidden" name="action" value="unban"><button type="submit">unban</button></form>
        {{else}}
        <form method="post" action="/admin/shadowban"><input type="hidden" name="id" value="{{.SteamID}}"><input type="hidden" name="action" value="ban"><button type="submit">shadowban</button></form>
        {{end}}
        <form method="post" action="/admin/connections/drop" onsubmit="return confirm('Drop this connection? The client will reconnect.')"><input type="hidden" name="id" value="{{.SteamID}}"><button type="submit">drop</button></form>
      </div>
    </td>
  </tr>
  {{else}}
  <tr><td colspan="6" class="muted">no live connections</td></tr>
  {{end}}
</table>

<h2>Shadowbans <span class="muted">· the silent-ban list. A banned account still connects and sees the world arm, but never scores, wins, or appears on a board, and its clicks are dropped — with nothing that reveals the ban. Takes effect on the player's next connect.</span></h2>
<table>
  <tr><th>player</th><th>reason</th><th>added (local)</th><th></th></tr>
  {{range .Shadowbans}}
  <tr>
    <td>{{plink .SteamID .SteamID}}</td>
    <td>{{if .Reason}}{{.Reason}}{{else}}<span class="muted">—</span>{{end}}</td>
    <td>{{lt .CreatedAt}}</td>
    <td><form method="post" action="/admin/shadowban"><input type="hidden" name="id" value="{{.SteamID}}"><input type="hidden" name="action" value="unban"><button type="submit">unban</button></form></td>
  </tr>
  {{else}}
  <tr><td colspan="4" class="muted">no shadowbans</td></tr>
  {{end}}
</table>
<form class="addbounty" method="post" action="/admin/shadowban">
  <h3>Shadowban a SteamID <span class="muted">· takes effect on their next connect</span></h3>
  <input type="hidden" name="action" value="ban">
  <input type="text" name="id" placeholder="SteamID64" required>
  <input type="text" name="reason" placeholder="reason (optional)" maxlength="200">
  <button type="submit">Shadowban</button>
</form>

<h2>Bounties <span class="muted">· the skin-to-win queue (times shown in your local zone; stored as UTC). When a bounty's win time passes, the player who won the most games during its window is recorded as the winner and the next bounty activates automatically. "hide" removes a bounty from the client (no active skin / not shown as a previous winner) without changing the queue — use it to test, then "unhide".</span></h2>
<table>
  <tr><th>skin</th><th>label</th><th>status</th><th>window (local)</th><th>winner</th><th></th></tr>
  {{range .Bounties}}
  <tr>
    <td>
      {{if .SkinImage}}<img class="skin" src="/admin/media?f={{.SkinImage}}" alt="">{{end}}
      {{if ne .Status "won"}}<input form="b{{.ID}}" type="file" name="skin" accept="image/*">
      <span class="inspect-edit"><input form="b{{.ID}}" type="text" name="inspect_link" value="{{.InspectLink}}" placeholder="inspect link (optional)" class="inspect" readonly><button type="button" class="editbtn" onclick="editInspect(this)">edit</button></span>{{else if .InspectLink}}<span class="muted">inspect link</span>{{end}}
    </td>
    <td>{{if eq .Status "won"}}{{.Label}}{{else}}<input form="b{{.ID}}" type="text" name="label" value="{{.Label}}" placeholder="label">{{end}}</td>
    <td><span class="pill {{.Status}}">{{.Status}}</span>{{if .Archived}} <span class="pill archived">hidden</span>{{end}}</td>
    <td>{{ltp .ActivatedAt}} &rarr; {{if eq .Status "won"}}{{lt .WinTime}}{{else}}<input form="b{{.ID}}" type="datetime-local" name="win_time" value="{{dtlocal .WinTime}}" data-utc="{{dtlocal .WinTime}}" required>{{end}}</td>
    <td>{{if eq .Status "won"}}{{if .WinnerID}}{{plink .WinnerID .WinnerName}} <span class="muted">({{.WinnerWins}})</span>{{else}}<span class="muted">no winner (empty window)</span>{{end}}{{else}}<span class="muted">—</span>{{end}}</td>
    <td>
      <div class="actions">
        {{if ne .Status "won"}}
        <form id="b{{.ID}}" method="post" action="/admin/bounties/edit" enctype="multipart/form-data"><input type="hidden" name="id" value="{{.ID}}"><button type="submit">save</button></form>
        {{if eq .Status "pending"}}<form method="post" action="/admin/bounties/delete" onsubmit="return confirm('Delete this bounty?')"><input type="hidden" name="id" value="{{.ID}}"><button type="submit">delete</button></form>{{end}}
        {{end}}
        {{if .Archived}}
        <form method="post" action="/admin/bounties/archive"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="archived" value="0"><button type="submit">unhide</button></form>
        {{else}}
        <form method="post" action="/admin/bounties/archive"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="archived" value="1"><button type="submit">hide</button></form>
        {{end}}
      </div>
    </td>
  </tr>
  {{else}}
  <tr><td colspan="6" class="muted">no bounties yet — add one below</td></tr>
  {{end}}
</table>
<form class="addbounty" method="post" action="/admin/bounties" enctype="multipart/form-data">
  <h3>Add bounty <span class="muted">· upload an image or paste a CS2 inspect link (or both — the image is the fallback if the link can't be fetched)</span></h3>
  <input type="file" name="skin" accept="image/*">
  <input type="text" name="inspect_link" placeholder="inspect link (steam://… or hex)" class="inspect">
  <input type="text" name="label" placeholder="label (optional)">
  <input type="datetime-local" name="win_time" required>
  <button type="submit">Add</button>
</form>

<h2>Recent games</h2>
<table>
  <tr><th>game</th><th>ended (local)</th><th>length</th><th class="num">rounds</th><th class="num">scorers</th><th class="num">clicks</th><th>winner</th></tr>
  {{range .Games}}
  <tr>
    <td><a class="mono" href="/admin/game?id={{.ID}}">{{short .ID}}</a></td>
    <td>{{lt .EndedAt}}</td>
    <td>{{dur .StartedAt .EndedAt}}</td>
    <td class="num">{{.Rounds}}</td>
    <td class="num">{{.Scorers}}</td>
    <td class="num">{{.Clicks}}</td>
    <td>{{if .WinnerID}}{{plink .WinnerID .WinnerName}}{{else}}<span class="muted">—</span>{{end}}</td>
  </tr>
  {{else}}
  <tr><td colspan="7" class="muted">no games in this view</td></tr>
  {{end}}
</table>
{{template "pager" .GamesPage}}

<h2>Anticheat <span class="muted">· most-recently flagged checks in this view, newest first. Click a player for their full per-event detail.</span></h2>
<table>
  <tr><th>when (local)</th><th>player</th><th>check</th><th>detail</th><th>game · round</th></tr>
  {{range .AntiCheat}}
  <tr>
    <td>{{lt .CreatedAt}}</td>
    <td>{{plink .SteamID .Name}}</td>
    <td class="mono">{{.Type}}</td>
    <td>{{.Detail}}</td>
    <td><a class="mono" href="/admin/game?id={{.GameID}}">{{short .GameID}}</a> · {{.RoundNo}}</td>
  </tr>
  {{else}}
  <tr><td colspan="5" class="muted">no anticheat activity in this view</td></tr>
  {{end}}
</table>
{{template "pager" .AntiCheatPage}}

<h2>Fastest clickers <span class="muted">· mean gap between clicks per round (first measured from arm), min 10 clicks</span></h2>
<table>
  <tr><th class="num">#</th><th>player</th><th class="num">clicks</th><th class="num">avg delta (ms)</th></tr>
  {{range $i, $e := .Fastest}}
  <tr><td class="num">{{add $.FastestPage.From $i}}</td><td>{{plink $e.SteamID $e.Name}}</td><td class="num">{{$e.Clicks}}</td><td class="num">{{printf "%.1f" $e.AvgDeltaMs}}</td></tr>
  {{else}}
  <tr><td colspan="4" class="muted">no players with 10+ scoring clicks in this view</td></tr>
  {{end}}
</table>
{{template "pager" .FastestPage}}

<h2>Leaderboards</h2>
<div class="cols">
  <div>
    <h2>Points</h2>
    <table><tr><th class="num">#</th><th>player</th><th class="num">points</th></tr>
    {{range $i, $e := .Points}}<tr><td class="num">{{add $.PointsPage.From $i}}</td><td>{{plink $e.SteamID $e.Username}}</td><td class="num">{{$e.Points}}</td></tr>{{else}}<tr><td colspan="3" class="muted">none</td></tr>{{end}}
    </table>
    {{template "pager" .PointsPage}}
  </div>
  <div>
    <h2>Hours won</h2>
    <table><tr><th class="num">#</th><th>player</th><th class="num">hours</th></tr>
    {{range $i, $e := .HoursWon}}<tr><td class="num">{{add $.HoursWonPage.From $i}}</td><td>{{plink $e.SteamID $e.Username}}</td><td class="num">{{$e.Points}}</td></tr>{{else}}<tr><td colspan="3" class="muted">none</td></tr>{{end}}
    </table>
    {{template "pager" .HoursWonPage}}
  </div>
  <div>
    <h2>Games won</h2>
    <table><tr><th class="num">#</th><th>player</th><th class="num">wins</th></tr>
    {{range $i, $e := .SessionsWon}}<tr><td class="num">{{add $.SessionsWonPage.From $i}}</td><td>{{plink $e.SteamID $e.Username}}</td><td class="num">{{$e.Points}}</td></tr>{{else}}<tr><td colspan="3" class="muted">none</td></tr>{{end}}
    </table>
    {{template "pager" .SessionsWonPage}}
  </div>
  <div>
    <h2>Top clickers</h2>
    <table><tr><th class="num">#</th><th>player</th><th class="num">clicks</th></tr>
    {{range $i, $e := .AllTime}}<tr><td class="num">{{add $.AllTimePage.From $i}}</td><td>{{plink $e.SteamID $e.Username}}</td><td class="num">{{$e.Points}}</td></tr>{{else}}<tr><td colspan="3" class="muted">none</td></tr>{{end}}
    </table>
    {{template "pager" .AllTimePage}}
  </div>
</div>
</div>
<script>
// Unlock a bounty's inspect-link field for editing (it's readonly by default so an
// unrelated save can't change the skin); the button removes itself once clicked.
function editInspect(btn){var i=btn.parentElement.querySelector('input.inspect');
  i.readOnly=false;i.focus();btn.remove();}
</script>
` + adminJS + `
</body></html>`))

var playerTmpl = template.Must(template.New("player").Funcs(adminFuncs).Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>player {{.Profile.Name}}</title><style>` + adminCSS + `</style></head><body><div class="wrap">
<a class="backlink" href="/admin">&larr; back to dashboard</a>
<div class="topbar">
  <div class="brand"><h1>{{.Profile.Name}}</h1><span class="badge mono">{{.Profile.Tag}}</span></div>
  <a class="logout" href="/admin/logout">log out</a>
</div>
<p class="muted">
  <a href="{{steamurl .Profile.SteamID}}" target="_blank" rel="noopener">Steam profile ↗</a>
  · <span class="mono">{{.Profile.SteamID}}</span>
  · first seen {{lt .Profile.CreatedAt}}
</p>

` + pagerTmpl + filterTmpl + `
{{define "filterhidden"}}<input type="hidden" name="id" value="{{.ID}}">{{end}}
{{template "filter" (dict "Action" "/admin/player" "Bounty" .Bounty "FilterLabel" .FilterLabel "Selectable" .Selectable "ID" .Profile.SteamID)}}

<div class="cards">
  <div class="card"><div class="n">{{.Profile.Points}}</div><div class="lbl">points</div></div>
  <div class="card"><div class="n">{{.Profile.Clicks}}</div><div class="lbl">scoring clicks</div></div>
  <div class="card"><div class="n">{{.Profile.GamesWon}}</div><div class="lbl">games won</div></div>
  <div class="card"><div class="n">{{.Profile.Checks}}</div><div class="lbl">checks flagged</div></div>
  <div class="card"><div class="n">{{.Profile.TestsPassed}}</div><div class="lbl">tests passed</div></div>
  <div class="card"><div class="n">{{.Profile.TestsFailed}}</div><div class="lbl">tests failed</div></div>
</div>

<h2>Anticheat status</h2>
{{if .Sanction.Active}}
<p class="muted">Live ladder for the active bounty <strong>{{.Sanction.BountyLabel}}</strong> <span class="mono">#{{.Sanction.BountyID}}</span> (counts reset each bounty).</p>
<div class="cards">
  <div class="card"><div class="n">
    {{if eq .Sanction.Status "live"}}<span class="ok">live</span>
    {{else if eq .Sanction.Status "cooldown"}}<span class="flag">cooldown</span>
    {{else}}<span class="err">ignored</span>{{end}}
  </div><div class="lbl">status</div></div>
  <div class="card"><div class="n">{{.Sanction.Checks}}</div><div class="lbl">flags this bounty</div></div>
</div>
{{if eq .Sanction.Status "cooldown"}}<p class="muted">cooldown ends {{ltp .Sanction.CooldownUntil}}</p>{{end}}
{{if eq .Sanction.Status "ignored"}}<p class="muted">ignored until the bounty resolves {{lt .Sanction.ResolveAt}}</p>{{end}}
<form class="addbounty" method="post" action="/admin/player/checks">
  <input type="hidden" name="id" value="{{.Profile.SteamID}}">
  <label>flag count <input type="number" name="checks" min="0" value="{{.Sanction.Checks}}" required></label>
  <button type="submit">save</button>
</form>
<p class="muted">Saving clears any active cooldown/ignored and re-baselines the ladder at the new count; it re-escalates from there and applies live.</p>
{{else}}
<p class="muted">No active bounty — the anticheat ladder is per-bounty, so there's nothing to set right now.</p>
{{end}}

<h2>Shadowban</h2>
{{if .Shadowbanned}}
<p class="muted">This account is <span class="err">silently banned</span> — on its next connect it sees the world arm but never scores, wins, or appears on a board, and its clicks are dropped. Already connected? Drop it from the dashboard's Connections panel to apply now.</p>
<form class="addbounty" method="post" action="/admin/shadowban">
  <input type="hidden" name="id" value="{{.Profile.SteamID}}">
  <input type="hidden" name="action" value="unban">
  <input type="hidden" name="back" value="player">
  <button type="submit">lift shadowban</button>
</form>
{{else}}
<p class="muted">Not shadowbanned. A shadowban takes effect on the player's next connect; drop their connection from the dashboard to force a reconnect.</p>
<form class="addbounty" method="post" action="/admin/shadowban">
  <input type="hidden" name="id" value="{{.Profile.SteamID}}">
  <input type="hidden" name="action" value="ban">
  <input type="hidden" name="back" value="player">
  <label>reason (optional) <input type="text" name="reason" maxlength="200"></label>
  <button type="submit">shadowban</button>
</form>
{{end}}

<h2>Recent games <span class="muted">· games this player scored in, newest first</span></h2>
<table>
  <tr><th>game</th><th>ended (local)</th><th class="num">rounds</th><th class="num">placement</th><th class="num">points</th><th class="num">scorers</th><th>winner</th></tr>
  {{range .Games}}
  <tr>
    <td><a class="mono" href="/admin/game?id={{.ID}}">{{short .ID}}</a></td>
    <td>{{lt .EndedAt}}</td>
    <td class="num">{{.Rounds}}</td>
    <td class="num">{{if eq .Placement 1}}<span class="ok">1st</span>{{else}}{{.Placement}}{{end}}</td>
    <td class="num">{{.Points}}</td>
    <td class="num">{{.Scorers}}</td>
    <td>{{if .WinnerID}}{{plink .WinnerID .WinnerName}}{{else}}<span class="muted">—</span>{{end}}</td>
  </tr>
  {{else}}
  <tr><td colspan="7" class="muted">no games in this view</td></tr>
  {{end}}
</table>
{{template "pager" .GamesPage}}

<h2>Anticheat checks</h2>
<table>
  <tr><th>when (local)</th><th>check</th><th>detail</th><th>game · round</th></tr>
  {{range .Checks}}
  <tr>
    <td>{{lt .CreatedAt}}</td>
    <td class="mono">{{.Type}}</td>
    <td>{{.Detail}}</td>
    <td><a class="mono" href="/admin/game?id={{.GameID}}">{{short .GameID}}</a> · {{.RoundNo}}</td>
  </tr>
  {{else}}
  <tr><td colspan="4" class="muted">no checks in this view</td></tr>
  {{end}}
</table>
{{template "pager" .ChecksPage}}

<h2>Anticheat tests</h2>
<table>
  <tr><th>sent (local)</th><th>kind</th><th>prompt</th><th>answer</th><th>result</th></tr>
  {{range .Tests}}
  <tr>
    <td>{{lt .SentAt}}</td>
    <td class="mono">{{.Kind}}</td>
    <td class="mono">{{.Prompt}}</td>
    <td class="mono">{{if .Answered}}{{.Answer}}{{else}}<span class="muted">—</span>{{end}}</td>
    <td>{{if not .Answered}}<span class="muted">pending</span>{{else if .Correct}}<span class="ok">correct</span>{{else}}<span class="err">wrong</span>{{end}}</td>
  </tr>
  {{else}}
  <tr><td colspan="5" class="muted">no tests in this view</td></tr>
  {{end}}
</table>
{{template "pager" .TestsPage}}
</div>` + adminJS + `</body></html>`))

var gameTmpl = template.Must(template.New("game").Funcs(adminFuncs).Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>game {{short .ID}}</title><style>` + adminCSS + `</style></head><body><div class="wrap">
<a class="backlink" href="/admin">&larr; back</a>
<h1>game <span class="mono">{{.ID}}</span></h1>
<p class="muted">started {{lt .StartedAt}} · ended {{lt .EndedAt}} · {{.Rounds}} rounds · {{dur .StartedAt .EndedAt}}</p>
{{range .RoundList}}
<h2>round {{.RoundNo}} <span class="muted">· N={{.N}} · {{.Players}} players · armed {{lt .ArmedAt}}</span>
  {{if .Checks}}<span class="flag">· {{len .Checks}} check(s) flagged</span>{{end}}</h2>
<table>
  <tr><th class="num">click N</th><th>player</th><th>steam id</th><th class="num">offset (ms)</th></tr>
  {{range .Clicks}}
  <tr><td class="num">{{.SlotNo}}</td><td>{{plink .SteamID .Name}}</td>
      <td class="mono">{{.SteamID}}</td><td class="num">{{.OffsetMs}}</td></tr>
  {{else}}
  <tr><td colspan="4" class="muted">no scoring clicks (race timed out)</td></tr>
  {{end}}
</table>
{{if .Checks}}
<table>
  <tr><th>anticheat check</th><th>player</th><th>detail</th></tr>
  {{range .Checks}}
  <tr><td class="mono flag">{{.Type}}</td><td>{{plink .SteamID .Name}}</td><td>{{.Detail}}</td></tr>
  {{end}}
</table>
{{end}}
{{end}}
</div>` + adminJS + `</body></html>`))
