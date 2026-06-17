package api

import (
	"crypto/subtle"
	"html/template"
	"net/http"
	"time"

	"github.com/gamah/splitclicker/internal/store"
	"go.uber.org/zap"
)

// adminAuth gates the admin views behind the ADMIN_PASSWORD set in the
// environment. When that password is unset the whole admin surface is disabled
// (404) so a misconfigured deploy can never expose an open admin. Otherwise it
// requires HTTP Basic auth (any username; the password must match, compared in
// constant time) and prompts the browser on failure. Returns true when the
// request may proceed.
func (h *handler) adminAuth(w http.ResponseWriter, r *http.Request) bool {
	if h.adminPassword == "" {
		http.NotFound(w, r) // feature disabled — don't reveal it exists
		return false
	}
	_, pass, ok := r.BasicAuth()
	if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(h.adminPassword)) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="splitclicker admin"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
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
	fastest, err := h.store.FastestClickers(ctx, 15)
	if err != nil {
		h.adminError(w, "load fastest clickers", err)
		return
	}
	data := dashboardData{
		Stats:       stats,
		Games:       games,
		Fastest:     fastest,
		Hourly:      h.cache.Hourly(15),
		HoursWon:    h.cache.HoursWon(15),
		SessionsWon: h.cache.SessionsWon(15),
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

type dashboardData struct {
	Stats       store.AdminStats
	Games       []store.AdminGame
	Fastest     []store.FastestClicker
	Hourly      []store.LeaderboardEntry
	HoursWon    []store.LeaderboardEntry
	SessionsWon []store.LeaderboardEntry
	Players     int
}

// adminFuncs are the template helpers shared by the admin views.
var adminFuncs = template.FuncMap{
	"ts": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") },
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
`

var dashboardTmpl = template.Must(template.New("dash").Funcs(adminFuncs).Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>splitclicker admin</title><style>` + adminCSS + `</style></head><body>
<h1>splitclicker admin</h1>
<div class="cards">
  <div class="card"><div class="n">{{.Players}}</div>players online</div>
  <div class="card"><div class="n">{{.Stats.Players}}</div>accounts</div>
  <div class="card"><div class="n">{{.Stats.Games}}</div>games</div>
  <div class="card"><div class="n">{{.Stats.Rounds}}</div>rounds</div>
  <div class="card"><div class="n">{{.Stats.Clicks}}</div>scoring clicks</div>
</div>

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

<h2>Fastest clickers <span class="muted">· mean arm&rarr;click delta, min 10 clicks, refreshed ~10 min</span></h2>
<table>
  <tr><th>#</th><th>player</th><th>clicks</th><th>avg delta (ms)</th></tr>
  {{range $i, $e := .Fastest}}
  <tr><td>{{add1 $i}}</td><td>{{plink $e.SteamID $e.Name}}</td><td>{{$e.Clicks}}</td><td>{{printf "%.1f" $e.AvgDeltaMs}}</td></tr>
  {{else}}
  <tr><td colspan="4" class="muted">no players with 10+ scoring clicks yet</td></tr>
  {{end}}
</table>

<div class="cols">
  <div>
    <h2>Hourly points</h2>
    <table><tr><th>#</th><th>player</th><th>points</th></tr>
    {{range $i, $e := .Hourly}}<tr><td>{{add1 $i}}</td><td>{{plink $e.SteamID $e.Username}}</td><td>{{$e.Points}}</td></tr>{{end}}
    </table>
  </div>
  <div>
    <h2>Hours won</h2>
    <table><tr><th>#</th><th>player</th><th>hours</th></tr>
    {{range $i, $e := .HoursWon}}<tr><td>{{add1 $i}}</td><td>{{plink $e.SteamID $e.Username}}</td><td>{{$e.Points}}</td></tr>{{end}}
    </table>
  </div>
  <div>
    <h2>Games won</h2>
    <table><tr><th>#</th><th>player</th><th>wins</th></tr>
    {{range $i, $e := .SessionsWon}}<tr><td>{{add1 $i}}</td><td>{{plink $e.SteamID $e.Username}}</td><td>{{$e.Points}}</td></tr>{{end}}
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
<h2>round {{.RoundNo}} <span class="muted">· N={{.N}} · {{.Players}} players · armed {{ts .ArmedAt}}</span></h2>
<table>
  <tr><th>click N</th><th>player</th><th>steam id</th><th>offset (ms)</th></tr>
  {{range .Clicks}}
  <tr><td>{{.SlotNo}}</td><td>{{plink .SteamID .Name}}</td>
      <td class="mono">{{.SteamID}}</td><td>{{.OffsetMs}}</td></tr>
  {{else}}
  <tr><td colspan="4" class="muted">no scoring clicks (race timed out)</td></tr>
  {{end}}
</table>
{{end}}
</body></html>`))
