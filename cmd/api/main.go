package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"grummans.finance.backend/internal/ledger"
	"grummans.finance.backend/internal/obligation"
	"grummans.finance.backend/internal/platform/httpapi"
)

func main() {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		slog.Error("DATABASE_URL is required")
		os.Exit(1)
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		slog.Error("connecting to database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	service := obligation.NewService(obligation.NewPostgresStore(pool))
	ledgerService := ledger.NewService(ledger.NewPostgresStore(pool))
	address := os.Getenv("BIND_ADDR")
	if address == "" {
		address = "127.0.0.1:8080"
	}
	server := &http.Server{
		Addr:              address,
		Handler:           httpapi.NewHandler(service, ledgerService),
		ReadHeaderTimeout: 5 * time.Second,
	}
	slog.Info("api listening", "addr", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("api stopped", "error", fmt.Errorf("serving HTTP: %w", err))
		os.Exit(1)
	}
}
