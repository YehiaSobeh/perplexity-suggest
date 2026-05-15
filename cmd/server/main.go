// Command server runs the Perplexity suggestion HTTP service.
//
//	GET /suggest?q=<query>  → autocomplete suggestions
//	GET /health             → liveness + upstream connection state
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yourname/perplexity-suggest/internal/config"
	"github.com/yourname/perplexity-suggest/internal/httpapi"
	"github.com/yourname/perplexity-suggest/internal/suggest/ws"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log := newLogger(cfg.LogLevel)
	log.Info("starting", "port", cfg.Port, "ws_url", cfg.WebSocketURL)

	// Root context is canceled on SIGINT / SIGTERM so the whole process
	// can drain in an orderly way.
	rootCtx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- upstream client ---
	client := ws.New(ws.Config{
		URL:                  cfg.WebSocketURL,
		Origin:               cfg.Origin,
		UserAgent:            cfg.UserAgent,
		DialTimeout:          cfg.DialTimeout,
		RequestTimeout:       cfg.RequestTimeout,
		MaxRetries:           cfg.MaxRetries,
		ReconnectDelay:       cfg.ReconnectDelay,
		MaxReconnectAttempts: cfg.MaxReconnectAttempts,
	}, log.With("component", "ws"))

	// Initial connect: log and move on if it fails — the reconnect loop
	// will keep trying and individual requests will return 503 in the
	// meantime. This is friendlier than refusing to start at all.
	dialCtx, cancel := context.WithTimeout(rootCtx, cfg.DialTimeout)
	if err := client.Connect(dialCtx); err != nil {
		log.Warn("initial WebSocket connect failed; will keep retrying", "err", err)
	}
	cancel()
	defer func() { _ = client.Close() }()

	// --- HTTP server ---
	handler := httpapi.NewHandler(client, client, log.With("component", "http"))
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           handler.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      cfg.RequestTimeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("HTTP server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	// Wait for shutdown signal or unrecoverable server error.
	select {
	case <-rootCtx.Done():
		log.Info("shutdown signal received")
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
	}

	// Graceful shutdown — finish in-flight requests, then close the socket.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown error", "err", err)
	}
	log.Info("stopped")
	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
