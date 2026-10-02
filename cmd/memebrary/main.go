package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/initialed85/memebrary-backend/internal/config"
	"github.com/initialed85/memebrary-backend/internal/describe"
	"github.com/initialed85/memebrary-backend/internal/httpapi"
	"github.com/initialed85/memebrary-backend/internal/store"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := config.Load()
	if err := os.MkdirAll(cfg.MediaDir, 0o755); err != nil {
		logger.Error("create media directory", "error", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		logger.Error("create database directory", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dataStore, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		logger.Error("open store", "error", err)
		os.Exit(1)
	}
	defer dataStore.Close()
	generator := describe.New(cfg.AIBaseURL, cfg.AIModel, cfg.AIAPIKey, dataStore)
	generator.Start(ctx)
	api := httpapi.New(dataStore, generator, cfg.MediaDir, cfg.MaxUploadSize, cfg.CORSOrigin, logger)
	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("memebrary listening", "addr", server.Addr, "media", cfg.MediaDir, "ai", generator.Enabled())
		serverErr <- server.ListenAndServe()
	}()
	select {
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server stopped", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("server shutdown", "error", err)
		}
	}
	fmt.Fprintln(os.Stdout, "memebrary stopped")
}
