package main

import (
	"context"
	"io"
	"log/slog"

	"github.com/AstRonin/radio-atlantida-backend/internal/config"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/migrations"
)

// migrateUp applies the embedded migrations and exits. It logs JSON to out
// only (not to LOG_ROOT): the migrate container runs once, and its output
// goes to the update log of the deploy script (§9.2).
func migrateUp(ctx context.Context, out io.Writer) error {
	cfg, err := config.LoadMigrate(".env")
	if err != nil {
		return err
	}
	log := logging.For(slog.New(logging.NewHandler(out, slog.LevelInfo)), logging.ComponentDB)

	if err := migrations.Up(ctx, cfg.DatabaseURL, log); err != nil {
		log.Error("migration failed", logging.KeyError, err)
		return err
	}
	return nil
}
