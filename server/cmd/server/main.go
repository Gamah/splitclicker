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
	"github.com/gamah/splitclicker/internal/store"
	"github.com/gamah/splitclicker/internal/ws"
	"go.uber.org/zap"
)

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

	// The hub and engine reference each other: build the hub first, give the
	// engine the hub as its Broadcaster, then wire the engine back into the hub.
	hub := ws.NewHub(log)
	engine := game.New(game.ConfigFromEnv(), hub, st, log)
	hub.SetEngine(engine)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go engine.Run(ctx)
	go runHourlyFinalizer(ctx, st, log)

	mux := api.NewRouter(st, hub, engine, log)

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
