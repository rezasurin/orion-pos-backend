// Command orion is the Orion POS backend: one binary for the API server and its maintenance
// commands.
//
//	orion serve                         run the HTTP API
//	orion migrate up|down|status        manage the database schema
//	orion version                       print the build version
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

	"github.com/rezasurin/orion-pos-backend/internal/config"
	"github.com/rezasurin/orion-pos-backend/internal/database"
	"github.com/rezasurin/orion-pos-backend/internal/httpserver"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: orion <command>

commands:
  serve                    run the HTTP API
  migrate up|down|status   manage the database schema
  version                  print the build version
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "orion:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("missing command")
	}
	if args[0] == "version" {
		fmt.Println(version)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := newLogger(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "serve":
		return serve(ctx, cfg, logger)
	case "migrate":
		return migrate(ctx, cfg, logger, args[1:])
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func newLogger(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var h slog.Handler = slog.NewJSONHandler(os.Stdout, opts)
	if cfg.LogFormat == "text" {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h).With("service", "orion", "env", cfg.Env, "version", version)
}

func migrate(ctx context.Context, cfg config.Config, logger *slog.Logger, args []string) error {
	if cfg.MigrateDatabaseURL == "" {
		return errors.New("ORION_MIGRATE_DATABASE_URL is required")
	}
	sub := "up"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "up":
		return database.Migrate(ctx, cfg.MigrateDatabaseURL, logger)
	case "down":
		return database.MigrateDown(ctx, cfg.MigrateDatabaseURL, logger)
	case "status":
		return database.MigrationStatus(ctx, cfg.MigrateDatabaseURL, logger)
	default:
		return fmt.Errorf("unknown migrate command %q (want up, down or status)", sub)
	}
}

func serve(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	if cfg.DatabaseURL == "" {
		return errors.New("ORION_DATABASE_URL is required")
	}
	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := database.CheckAppRole(ctx, pool); err != nil {
		return err
	}

	expected, err := database.ExpectedVersion()
	if err != nil {
		return err
	}

	useSentry := cfg.SentryDSN != ""
	if useSentry {
		flush, err := httpserver.InitSentry(cfg.SentryDSN, cfg.Env, version)
		if err != nil {
			return fmt.Errorf("sentry: %w", err)
		}
		defer flush()
	}

	router := httpserver.NewRouter(httpserver.Options{
		Logger:    logger,
		UseSentry: useSentry,
		Ready: map[string]httpserver.ReadinessCheck{
			"database": func(ctx context.Context) error { return pool.Ping(ctx) },
			// The schema may be newer than this binary (expand-then-contract migrations allow a
			// rollback), but never older.
			"migrations": func(ctx context.Context) error {
				applied, err := database.AppliedVersion(ctx, pool)
				if err != nil {
					return err
				}
				if applied < expected {
					return fmt.Errorf("schema at version %d, binary needs %d", applied, expected)
				}
				return nil
			},
		},
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.HTTPAddr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
