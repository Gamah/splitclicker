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
