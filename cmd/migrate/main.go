package main

import (
	"errors"
	"log/slog"
	"os"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

func main() {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		slog.Error("DATABASE_URL is required")
		os.Exit(1)
	}
	migrationsPath := os.Getenv("MIGRATIONS_PATH")
	if migrationsPath == "" {
		migrationsPath = "file://db/migrations"
	}

	migrations, err := migrate.New(migrationsPath, databaseURL)
	if err != nil {
		slog.Error("opening migrations", "error", err)
		os.Exit(1)
	}
	defer migrations.Close()
	if err := migrations.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		slog.Error("applying migrations", "error", err)
		os.Exit(1)
	}
	slog.Info("migrations are current")
}
