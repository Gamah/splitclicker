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
	setDurSec(f.ArmMinSec, &c.ArmMin)
	setDurSec(f.ArmMaxSec, &c.ArmMax)
	setInt(f.ClicksPerPlayer, &c.ClicksPerPlayer)
	setInt(f.MinClicks, &c.MinClicks)
	setInt(f.RoundsPerGame, &c.RoundsPerGame)
	setDurMs(f.RaceMaxMs, &c.RaceMax)
	setDurMs(f.ResultDisplayMs, &c.ResultDisplay)
	setDurMs(f.IntermissionMs, &c.Intermission)
	setInt(f.BoardSize, &c.BoardSize)
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
