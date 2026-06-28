package main

import (
	"context"
	"embed"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/remvze/moodist/internal/server"
)

// dist is the Astro build output, embedded at compile time.
// Run `make build-web` (or `pnpm build` inside web/) before `go build`.
//
//go:embed all:dist
var dist embed.FS

func main() {
	// ── flags & env ────────────────────────────────────────────────────────
	addr := flag.String("addr", server.EnvOrDefault("MOODIST_ADDR", ":8080"), "TCP address to listen on")
	logLevel := flag.String("log-level", server.EnvOrDefault("MOODIST_LOG_LEVEL", "info"), "Log level: debug|info|warn|error")
	flag.Parse()

	// ── logger ─────────────────────────────────────────────────────────────
	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	// ── server ─────────────────────────────────────────────────────────────
	cfg := server.DefaultConfig()
	cfg.Addr = *addr
	cfg.Logger = log

	srv, err := server.New(dist, cfg)
	if err != nil {
		log.Error("failed to create server", "err", err)
		os.Exit(1)
	}

	// ── run ────────────────────────────────────────────────────────────────
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	// ── shutdown ───────────────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		log.Info("signal received", "signal", sig.String())
	case err := <-errCh:
		log.Error("server error", "err", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Error("shutdown error", "err", err)
		os.Exit(1)
	}

	log.Info("moodist stopped cleanly")
}
