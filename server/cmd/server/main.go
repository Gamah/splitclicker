package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gamah/splitclicker/internal/api"
	"github.com/gamah/splitclicker/internal/db"
	"github.com/gamah/splitclicker/internal/game"
	"github.com/gamah/splitclicker/internal/runtimecfg"
	"github.com/gamah/splitclicker/internal/store"
	"github.com/gamah/splitclicker/internal/ws"
	"go.uber.org/zap"
)

// gameConfig builds the engine tuning with precedence default < env < config.json
// (data/config.json). The game tunables are read once here at startup, so editing
// them in config.json takes effect on the next restart (the skin/countdown, by
// contrast, are re-read per request and apply live).
func gameConfig() game.Config {
	c := game.ConfigFromEnv()
	f := runtimecfg.Load()
	setDurSec := func(p *int, d *time.Duration) {
		if p != nil {
			*d = time.Duration(*p) * time.Second
		}
	}
	setDurMs := func(p *int, d *time.Duration) {
		if p != nil {
			*d = time.Duration(*p) * time.Millisecond
		}
	}
	setInt := func(p *int, i *int) {
		if p != nil {
			*i = *p
		}
	}
	setFloat := func(p *float64, f *float64) {
		if p != nil {
			*f = *p
		}
	}
	setDurSec(f.ArmMinSec, &c.ArmMin)
	setDurSec(f.ArmMaxSec, &c.ArmMax)
	setInt(f.ClicksPerPlayer, &c.ClicksPerPlayer)
	setInt(f.MinClicks, &c.MinClicks)
	setInt(f.RoundsPerGame, &c.RoundsPerGame)
	setInt(f.ButtonsOnScreen, &c.ButtonsOnScreen)
	setDurMs(f.RaceMaxMs, &c.RaceMax)
	setDurMs(f.ResultDisplayMs, &c.ResultDisplay)
	setDurMs(f.IntermissionMs, &c.Intermission)
	setInt(f.BoardSize, &c.BoardSize)
	setInt(f.TickHz, &c.TickHz)
	setInt(f.TickSampleK, &c.TickSampleK)
	setInt(f.FastClickMs, &c.FastClickMs)
	setFloat(f.MaxClickFactor, &c.MaxClickFactor)
	setInt(f.SoloLeadMargin, &c.SoloLeadMargin)
	setInt(f.DominantRunnerUpMin, &c.DominantRunnerUpMin)
	setInt(f.AfkCheck, &c.AfkCheck)
	setInt(f.ReactionMinMs, &c.ReactionMinMs)
	setInt(f.ImpossibleLatency, &c.ImpossibleLatency)
	setInt(f.MetronomeMinClicks, &c.MetronomeMinClicks)
	setFloat(f.MetronomeMaxCV, &c.MetronomeMaxCV)
	setInt(f.TouchCheck, &c.TouchCheck)
	setFloat(f.StraightPathRatio, &c.StraightPathRatio)
	setInt(f.StraightPathMin, &c.StraightPathMin)
	setInt(f.CheckCooldownThreshold, &c.CheckCooldownThreshold)
	setInt(f.CheckCooldownMins, &c.CheckCooldownMins)
	setInt(f.CheckIgnoreAfter, &c.CheckIgnoreAfter)
	return c
}

// version is injected at build time via -ldflags "-X main.version=<git-hash>".
var version = "dev"

// runHourlyFinalizer credits the winner of each completed UTC clock-hour to the
// "hours won" board. It runs once at startup (catching up any hours missed while
// the process was down) and again just after every hour boundary. FinalizeDueHours
// is idempotent, so the small post-boundary delay and the startup pass can't
// double-count.
func runHourlyFinalizer(ctx context.Context, st *store.Store, log *zap.Logger) {
	finalize := func() {
		fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		n, err := st.FinalizeDueHours(fctx, time.Now())
		if err != nil {
			log.Error("finalize hours", zap.Error(err))
		} else if n > 0 {
			log.Info("finalized hourly winners", zap.Int("hours", n))
		}
	}

	finalize() // catch up on boundaries crossed while we were down
	for {
		now := time.Now().UTC()
		// Wake a touch after the next hour boundary so the just-ended hour is closed.
		next := now.Truncate(time.Hour).Add(time.Hour + 5*time.Second)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			finalize()
		}
	}
}

// runBountyFinalizer advances the skin-to-win bounty queue: once an active
// bounty's win_time passes it records the window's winner and activates the next
// queued bounty, so the game keeps ticking forward. It runs once at startup
// (catching up any deadlines crossed while the process was down, and activating
// the first pending bounty) and then on a short tick. FinalizeDueBounties is
// transactional and idempotent on the queue state, so re-runs can't double-count.
func runBountyFinalizer(ctx context.Context, st *store.Store, cache *store.LeaderboardCache, hub *ws.Hub, log *zap.Logger) {
	finalize := func() {
		fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		// Snapshot the active bounty id before/after: it changes on a finalize+promote,
		// a first-ever/after-drain promotion, and a drain to none — every case the
		// client must refresh its skin/countdown for. (FinalizeDueBounties only counts
		// finalizes, so the id diff is the broader, correct trigger.)
		before, _, _ := st.ActiveBounty(fctx)
		n, err := st.FinalizeDueBounties(fctx, time.Now())
		if err != nil {
			log.Error("finalize bounties", zap.Error(err))
			return
		}
		after, _, _ := st.ActiveBounty(fctx)
		if n > 0 {
			log.Info("finalized bounties", zap.Int("count", n))
		}
		if before.ID != after.ID {
			// The active bounty flipped: the windowed boards now scope to the new
			// window, so refresh the cache immediately rather than waiting for the next
			// game end, and nudge every client to re-fetch its bounty state so nobody
			// sits in the stale post-rollover view.
			if err := cache.Refresh(fctx); err != nil {
				log.Error("refresh leaderboard cache after bounty advance", zap.Error(err))
			}
			hub.BroadcastBountyUpdate()
		}
	}

	finalize() // catch up + activate the first pending bounty
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			finalize()
		}
	}
}

// runFastestClickersRefresh recomputes the fastest_clickers materialized view
// (the admin "fastest clickers" board) once at startup and then every 10 minutes,
// so that admin page reads stay cheap and the board is at most ~10 min stale.
func runFastestClickersRefresh(ctx context.Context, st *store.Store, log *zap.Logger) {
	refresh := func() {
		rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		if err := st.RefreshFastestClickers(rctx); err != nil {
			log.Error("refresh fastest clickers", zap.Error(err))
		}
	}
	refresh()
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refresh()
		}
	}
}

func main() {
	log, _ := zap.NewProduction()
	defer log.Sync()
	log.Info("splitclicker starting", zap.String("version", version))

	dsn := os.Getenv("DATABASE_URL")
	pool, err := db.Connect(dsn)
	if err != nil {
		log.Fatal("connect database", zap.Error(err))
	}
	defer pool.Close()
	if err := db.Migrate(dsn); err != nil {
		log.Fatal("migrate", zap.Error(err))
	}

	st := store.New(pool)

	// The leaderboard boards are served from this in-memory cache so the API
	// never hits Postgres per request. It's refreshed once per game ("session")
	// via the engine's game-end hook below (and once now, before serving).
	cache := store.NewLeaderboardCache(st)

	// The hub and engine reference each other: build the hub first, give the
	// engine the hub as its Broadcaster, then wire the engine back into the hub.
	hub := ws.NewHub(log)
	engine := game.New(gameConfig(), hub, st, log)
	hub.SetEngine(engine)
	// Re-read the host-editable dev note from config.json once per game.
	engine.SetDevNoteFn(func() string { return runtimecfg.Load().DevNote })
	// The anticheat checks + sanction ladder need the active bounty snapshot: its id
	// (scopes the ladder), winner-lock time (the "ignored" countdown), and the
	// games-won leader + margin (the session-level solo_round check). All read from
	// the in-memory cache, so it's free to call once per game.
	engine.SetBountyInfoFn(func() game.BountyInfo {
		id, resolveMs, ok := cache.ActiveBountyMeta()
		bi := game.BountyInfo{ID: id, ResolveAtMs: resolveMs, Active: ok}
		if sw := cache.SessionsWon(2); len(sw) > 0 {
			bi.LeaderID = sw[0].SteamID
			// Lead = gap over the runner-up; when alone on the board no runner-up
			// exists, so the lead IS the leader's own total. The session-level
			// solo_round check keys off this margin, not how many players are connected.
			bi.LeadMargin = sw[0].Points
			if len(sw) > 1 {
				bi.LeadMargin = sw[0].Points - sw[1].Points
			}
		}
		return bi
	})
	// The arming-phase AFK pass (issue #43) reads cursor movement during arming for the
	// v7+ roster (v6 sends cursors armed-only, so the hub omits it here — it can't be
	// arming-judged). Evaluated after pending() returns, before the button arms.
	engine.SetArmingCursorActivityFn(hub.ArmingCursorActivity)
	// The round-end whole-round cursor activity (arming + armed) feeds the score-aware
	// checks: busted (scored with no cursor all round) and the path/cadence signals. This
	// one includes v6 (it sends armed cursors, so busted won't false-fire on it).
	engine.SetAllCursorActivityFn(hub.AllCursorActivity)
	// Per-window touch data (the `touch {id}` first-entry stamps) feeds the no_hover /
	// fast_hover dwell checks. Keyed by SteamID → button id → ms-since-arm.
	engine.SetTouchDataFn(hub.AllTouchData)
	// Per-connection minimum observed ping RTT feeds impossible_latency: a scoring click
	// faster than the connection's own round-trip is physically impossible.
	engine.SetMinRTTFn(hub.MinRTTms)
	// The whole game's cursor paths feed the durable replay (admin viewer), captured the
	// same per-window way as the afk pass but kept as the full sample list.
	engine.SetCursorTracksFn(hub.AllCursorTracks)
	engine.SetGameEndHook(func(ctx context.Context) {
		if err := cache.Refresh(ctx); err != nil {
			log.Error("refresh leaderboard cache", zap.Error(err))
		}
	})

	// Populate the cache before serving so the first reads aren't empty.
	{
		ictx, icancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := cache.Refresh(ictx); err != nil {
			log.Error("initial leaderboard cache refresh", zap.Error(err))
		}
		icancel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go engine.Run(ctx)
	go runHourlyFinalizer(ctx, st, log)
	go runBountyFinalizer(ctx, st, cache, hub, log)
	go runFastestClickersRefresh(ctx, st, log)

	mux := api.NewRouter(st, cache, hub, engine, log)

	port := os.Getenv("PORT")
	if port == "" {
		port = "6969"
	}
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%s", port),
		Handler:           api.CORSMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Info("listening", zap.String("port", port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("server error", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutting down")
	cancel() // stop the game loop

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Error("shutdown error", zap.Error(err))
	}
	log.Info("stopped")
}
