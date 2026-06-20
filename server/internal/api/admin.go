package api

import (
	"crypto/subtle"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gamah/splitclicker/internal/store"
	"go.uber.org/zap"
)

// adminSessionTTL bounds how long an admin login lasts before re-auth is needed.
const adminSessionTTL = 12 * time.Hour

// adminCookieName is the session cookie set after a successful login.
const adminCookieName = "admin_session"

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

// GET /admin — dashboard: history counts, the live leaderboards, and the most
// recent games (each linking to its per-click detail).
func (h *handler) adminDashboard(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuth(w, r) {
		return
	}
	ctx := r.Context()
	stats, err := h.store.AdminStats(ctx)
	if err != nil {
		h.adminError(w, "load stats", err)
		return
	}
	games, err := h.store.RecentGames(ctx, 50)
	if err != nil {
		h.adminError(w, "load recent games", err)
		return
	}
	bounties, err := h.store.ListBounties(ctx)
	if err != nil {
		h.adminError(w, "load bounties", err)
		return
	}
	fastest, err := h.store.FastestClickers(ctx, 15)
	if err != nil {
		h.adminError(w, "load fastest clickers", err)
		return
	}
	checks, err := h.store.RecentChecks(ctx, 50)
	if err != nil {
		h.adminError(w, "load anticheat checks", err)
		return
	}
	tests, err := h.store.RecentTests(ctx, 50)
	if err != nil {
		h.adminError(w, "load anticheat tests", err)
		return
	}
	data := dashboardData{
		Stats:       stats,
		Games:       games,
		Bounties:    bounties,
		Fastest:     fastest,
		Checks:      checks,
		Tests:       tests,
		Hourly:      h.cache.Hourly(15),
		HoursWon:    h.cache.HoursWon(15),
		SessionsWon: h.cache.SessionsWon(15),
		AllTime:     h.cache.AllTimeClickers(15),
		Players:     h.hub.PlayerCount(),
	}
	h.renderAdmin(w, dashboardTmpl, data)
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

type loginData struct{ Error string }

var loginTmpl = template.Must(template.New("login").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>splitclicker admin · login</title><style>` + adminCSS + `</style></head><body>
<form class="login" method="post" action="/admin/login">
  <h1>admin</h1>
  {{if .Error}}<p class="err">{{.Error}}</p>{{end}}
  <input type="password" name="password" placeholder="password" autofocus autocomplete="current-password">
  <button type="submit">Login</button>
</form>
</body></html>`))

type dashboardData struct {
	Stats       store.AdminStats
	Games       []store.AdminGame
	Bounties    []store.Bounty
	Fastest     []store.FastestClicker
	Checks      []store.AdminCheck
	Tests       []store.AdminTest
	Hourly      []store.LeaderboardEntry
	HoursWon    []store.LeaderboardEntry
	SessionsWon []store.LeaderboardEntry
	AllTime     []store.LeaderboardEntry
	Players     int
}

// adminFuncs are the template helpers shared by the admin views.
var adminFuncs = template.FuncMap{
	"ts": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") },
	// tsp renders a nullable timestamp ("—" when unset, e.g. a pending bounty's
	// activation time).
	"tsp": func(t *time.Time) string {
		if t == nil {
			return "—"
		}
		return t.UTC().Format("2006-01-02 15:04:05")
	},
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
	"add1": func(i int) int { return i + 1 }, // 0-based range index → 1-based rank
	// plink renders a player name as a link to their public Steam profile. The
	// name (a user-controlled display string) is HTML-escaped; an empty steam id
	// or name degrades gracefully. Returns template.HTML so it isn't re-escaped.
	"plink": func(steamID, name string) template.HTML {
		if name == "" {
			name = "anon"
		}
		safeName := template.HTMLEscapeString(name)
		if steamID == "" {
			return template.HTML(safeName)
		}
		url := "https://steamcommunity.com/profiles/" + template.HTMLEscapeString(steamID)
		return template.HTML(`<a href="` + url + `" target="_blank" rel="noopener">` + safeName + `</a>`)
	},
}

const adminCSS = `
body{font:14px/1.5 system-ui,sans-serif;margin:2rem;color:#1a1a1a;background:#fafafa}
h1,h2{font-weight:600}h2{margin-top:2rem}
a{color:#0a58ca;text-decoration:none}a:hover{text-decoration:underline}
table{border-collapse:collapse;width:100%;margin-top:.5rem;background:#fff}
th,td{padding:.35rem .6rem;border:1px solid #e2e2e2;text-align:left}
th{background:#f0f0f0}
.cards{display:flex;gap:1rem;flex-wrap:wrap}
.card{background:#fff;border:1px solid #e2e2e2;border-radius:8px;padding:.8rem 1.2rem;min-width:7rem}
.card .n{font-size:1.6rem;font-weight:700}
.cols{display:flex;gap:2rem;flex-wrap:wrap}.cols>div{flex:1;min-width:18rem}
.muted{color:#888}.mono{font-family:ui-monospace,monospace}
.err{color:#b00020;margin:.5rem 0}
.ok{color:#0a7a28}
.flag{color:#b00020;font-weight:600}
.login{max-width:20rem;margin:6rem auto;background:#fff;border:1px solid #e2e2e2;border-radius:8px;padding:1.5rem}
.login input{width:100%;padding:.5rem;margin:.4rem 0;box-sizing:border-box;font-size:1rem}
.login button{width:100%;padding:.5rem;font-size:1rem;cursor:pointer}
.logout{float:right;font-size:.85rem}
img.skin{height:40px;border-radius:4px;display:block;background:#eee}
td input,td button{font-size:.85rem}
td input[type=text],td input[type=datetime-local]{padding:.2rem}
.addbounty{margin-top:1rem;padding:1rem;background:#fff;border:1px solid #e2e2e2;border-radius:8px;display:flex;gap:.6rem;align-items:center;flex-wrap:wrap}
.addbounty h3{margin:0 .5rem 0 0;font-weight:600;font-size:1rem}
.actions{display:flex;gap:.4rem}
`

var dashboardTmpl = template.Must(template.New("dash").Funcs(adminFuncs).Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>splitclicker admin</title><style>` + adminCSS + `</style></head><body>
<a class="logout" href="/admin/logout">log out</a>
<h1>splitclicker admin</h1>
<div class="cards">
  <div class="card"><div class="n">{{.Players}}</div>players online</div>
  <div class="card"><div class="n">{{.Stats.Players}}</div>accounts</div>
  <div class="card"><div class="n">{{.Stats.Games}}</div>games</div>
  <div class="card"><div class="n">{{.Stats.Rounds}}</div>rounds</div>
  <div class="card"><div class="n">{{.Stats.Clicks}}</div>scoring clicks</div>
  <div class="card"><div class="n">{{.Stats.Checks}}</div>anticheat checks</div>
  <div class="card"><div class="n">{{.Stats.Tests}}</div>tests sent</div>
</div>

<h2>Bounties <span class="muted">· the skin-to-win queue (times UTC). When a bounty's win time passes, the player who won the most games during its window is recorded as the winner and the next bounty activates automatically.</span></h2>
<table>
  <tr><th>skin</th><th>label</th><th>status</th><th>window (UTC)</th><th>winner</th><th></th></tr>
  {{range .Bounties}}
  <tr>
    <td>
      <img class="skin" src="/admin/media?f={{.SkinImage}}" alt="">
      {{if ne .Status "won"}}<input form="b{{.ID}}" type="file" name="skin" accept="image/*">{{end}}
    </td>
    <td>{{if eq .Status "won"}}{{.Label}}{{else}}<input form="b{{.ID}}" type="text" name="label" value="{{.Label}}" placeholder="label">{{end}}</td>
    <td>{{.Status}}</td>
    <td>{{tsp .ActivatedAt}} &rarr; {{if eq .Status "won"}}{{ts .WinTime}}{{else}}<input form="b{{.ID}}" type="datetime-local" name="win_time" value="{{dtlocal .WinTime}}" required>{{end}}</td>
    <td>{{if eq .Status "won"}}{{if .WinnerID}}{{plink .WinnerID .WinnerName}} <span class="muted">({{.WinnerWins}})</span>{{else}}<span class="muted">no winner (empty window)</span>{{end}}{{else}}<span class="muted">—</span>{{end}}</td>
    <td>
      {{if ne .Status "won"}}
      <div class="actions">
        <form id="b{{.ID}}" method="post" action="/admin/bounties/edit" enctype="multipart/form-data"><input type="hidden" name="id" value="{{.ID}}"><button type="submit">save</button></form>
        {{if eq .Status "pending"}}<form method="post" action="/admin/bounties/delete" onsubmit="return confirm('Delete this bounty?')"><input type="hidden" name="id" value="{{.ID}}"><button type="submit">delete</button></form>{{end}}
      </div>
      {{end}}
    </td>
  </tr>
  {{else}}
  <tr><td colspan="6" class="muted">no bounties yet — add one below</td></tr>
  {{end}}
</table>
<form class="addbounty" method="post" action="/admin/bounties" enctype="multipart/form-data">
  <h3>Add bounty</h3>
  <input type="file" name="skin" accept="image/*" required>
  <input type="text" name="label" placeholder="label (optional)">
  <input type="datetime-local" name="win_time" required>
  <button type="submit">Add</button>
</form>

<h2>Recent games</h2>
<table>
  <tr><th>game</th><th>ended (UTC)</th><th>length</th><th>rounds</th><th>scorers</th><th>clicks</th><th>winner</th></tr>
  {{range .Games}}
  <tr>
    <td><a class="mono" href="/admin/game?id={{.ID}}">{{short .ID}}</a></td>
    <td>{{ts .EndedAt}}</td>
    <td>{{dur .StartedAt .EndedAt}}</td>
    <td>{{.Rounds}}</td>
    <td>{{.Scorers}}</td>
    <td>{{.Clicks}}</td>
    <td>{{if .WinnerID}}{{plink .WinnerID .WinnerName}}{{else}}<span class="muted">—</span>{{end}}</td>
  </tr>
  {{else}}
  <tr><td colspan="7" class="muted">no games recorded yet</td></tr>
  {{end}}
</table>

<h2>Fastest clickers <span class="muted">· mean gap between clicks per round (first measured from arm), min 10 clicks, refreshed ~10 min</span></h2>
<table>
  <tr><th>#</th><th>player</th><th>clicks</th><th>avg delta (ms)</th></tr>
  {{range $i, $e := .Fastest}}
  <tr><td>{{add1 $i}}</td><td>{{plink $e.SteamID $e.Name}}</td><td>{{$e.Clicks}}</td><td>{{printf "%.1f" $e.AvgDeltaMs}}</td></tr>
  {{else}}
  <tr><td colspan="4" class="muted">no players with 10+ scoring clicks yet</td></tr>
  {{end}}
</table>

<h2>Anticheat checks <span class="muted">· rounds the end-of-round checks flagged (fast_clicks &lt;130ms · too_many_clicks &gt;2× per-player · solo_round lone leader · dominant_winner &gt;2× runner-up). A test-capable (v3+) flagged player is benched until they pass a test.</span></h2>
<table>
  <tr><th>when (UTC)</th><th>player</th><th>check</th><th>detail</th><th>game · round</th></tr>
  {{range .Checks}}
  <tr>
    <td>{{ts .CreatedAt}}</td>
    <td>{{plink .SteamID .Name}}</td>
    <td class="mono">{{.Type}}</td>
    <td>{{.Detail}}</td>
    <td><a class="mono" href="/admin/game?id={{.GameID}}">{{short .GameID}}</a> · {{.RoundNo}}</td>
  </tr>
  {{else}}
  <tr><td colspan="5" class="muted">no checks flagged yet</td></tr>
  {{end}}
</table>

<h2>Anticheat tests <span class="muted">· every test sent to a flagged player and the answer received</span></h2>
<table>
  <tr><th>sent (UTC)</th><th>player</th><th>kind</th><th>prompt</th><th>answer</th><th>result</th></tr>
  {{range .Tests}}
  <tr>
    <td>{{ts .SentAt}}</td>
    <td>{{plink .SteamID .Name}}</td>
    <td class="mono">{{.Kind}}</td>
    <td class="mono">{{.Prompt}}</td>
    <td class="mono">{{if .Answered}}{{.Answer}}{{else}}<span class="muted">—</span>{{end}}</td>
    <td>{{if not .Answered}}<span class="muted">pending</span>{{else if .Correct}}<span class="ok">correct</span>{{else}}<span class="err">wrong</span>{{end}}</td>
  </tr>
  {{else}}
  <tr><td colspan="6" class="muted">no tests sent yet</td></tr>
  {{end}}
</table>

<h2>Leaderboards <span class="muted">· the first three are scoped to the active bounty's window (reset when it's won); all-time clickers spans the whole DB.</span></h2>
<div class="cols">
  <div>
    <h2>Points (this bounty)</h2>
    <table><tr><th>#</th><th>player</th><th>points</th></tr>
    {{range $i, $e := .Hourly}}<tr><td>{{add1 $i}}</td><td>{{plink $e.SteamID $e.Username}}</td><td>{{$e.Points}}</td></tr>{{end}}
    </table>
  </div>
  <div>
    <h2>Hours won (this bounty)</h2>
    <table><tr><th>#</th><th>player</th><th>hours</th></tr>
    {{range $i, $e := .HoursWon}}<tr><td>{{add1 $i}}</td><td>{{plink $e.SteamID $e.Username}}</td><td>{{$e.Points}}</td></tr>{{end}}
    </table>
  </div>
  <div>
    <h2>Games won (this bounty)</h2>
    <table><tr><th>#</th><th>player</th><th>wins</th></tr>
    {{range $i, $e := .SessionsWon}}<tr><td>{{add1 $i}}</td><td>{{plink $e.SteamID $e.Username}}</td><td>{{$e.Points}}</td></tr>{{end}}
    </table>
  </div>
  <div>
    <h2>All-time clickers</h2>
    <table><tr><th>#</th><th>player</th><th>clicks</th></tr>
    {{range $i, $e := .AllTime}}<tr><td>{{add1 $i}}</td><td>{{plink $e.SteamID $e.Username}}</td><td>{{$e.Points}}</td></tr>{{end}}
    </table>
  </div>
</div>
</body></html>`))

var gameTmpl = template.Must(template.New("game").Funcs(adminFuncs).Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>game {{short .ID}}</title><style>` + adminCSS + `</style></head><body>
<p><a href="/admin">&larr; back</a></p>
<h1>game <span class="mono">{{.ID}}</span></h1>
<p class="muted">started {{ts .StartedAt}} · ended {{ts .EndedAt}} · {{.Rounds}} rounds · {{dur .StartedAt .EndedAt}}</p>
{{range .RoundList}}
<h2>round {{.RoundNo}} <span class="muted">· N={{.N}} · {{.Players}} players · armed {{ts .ArmedAt}}</span>
  {{if .Checks}}<span class="flag">· {{len .Checks}} check(s) flagged</span>{{end}}</h2>
<table>
  <tr><th>click N</th><th>player</th><th>steam id</th><th>offset (ms)</th></tr>
  {{range .Clicks}}
  <tr><td>{{.SlotNo}}</td><td>{{plink .SteamID .Name}}</td>
      <td class="mono">{{.SteamID}}</td><td>{{.OffsetMs}}</td></tr>
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
</body></html>`))
