// Command orion is the Orion POS backend: one binary for the API server and its maintenance
// commands.
//
//	orion serve                         run the HTTP API
//	orion worker                        run background jobs (email, token cleanup)
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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/rezasurin/orion-pos-backend/internal/api"
	"github.com/rezasurin/orion-pos-backend/internal/catalog"
	"github.com/rezasurin/orion-pos-backend/internal/config"
	"github.com/rezasurin/orion-pos-backend/internal/database"
	"github.com/rezasurin/orion-pos-backend/internal/entitlements"
	"github.com/rezasurin/orion-pos-backend/internal/httpserver"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/notify"
	"github.com/rezasurin/orion-pos-backend/internal/platform"
	"github.com/rezasurin/orion-pos-backend/internal/sales"
	"github.com/rezasurin/orion-pos-backend/internal/sync"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: orion <command>

commands:
  serve                    run the HTTP API
  worker                   run background jobs
  admin <command>          operator commands: create-tenant, seed-demo
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
	case "worker":
		return worker(ctx, cfg, logger)
	case "admin":
		return admin(ctx, cfg, logger, args[1:])
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

	tenants := tenancy.NewService(pool)
	ents := entitlements.NewResolver(pool, kernel.SystemClock{})
	// The API only inserts jobs, in the same transaction as the write that needs them; the
	// worker process runs them. A client without queues is insert-only.
	jobs, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		return fmt.Errorf("river: %w", err)
	}
	tenantKeys, deviceKeys, err := keyrings(cfg, logger)
	if err != nil {
		return err
	}
	ids, err := identity.NewService(identity.Deps{
		Pool: pool, Clock: kernel.SystemClock{}, TenantKeys: tenantKeys, DeviceKeys: deviceKeys,
		Jobs: jobs, Gate: tenants.CheckActive, Entitlements: ents,
	})
	if err != nil {
		return err
	}
	catalogSvc := catalog.NewService(pool)
	syncSvc, err := sync.NewService(sync.Deps{
		Pool: pool, Identity: ids, Clock: kernel.SystemClock{}, Logger: logger, Catalog: catalogSvc, Tenancy: tenants, Entitlements: ents,
		Projectors: []sync.Projector{sales.NewProjector(sales.Deps{Identity: ids, Tenancy: tenants, Catalog: catalogSvc})},
	})
	if err != nil {
		return err
	}
	var plat *platform.Service
	if cfg.PlatformDatabaseURL != "" {
		pp, err := database.Connect(ctx, cfg.PlatformDatabaseURL)
		if err != nil {
			return err
		}
		defer pp.Close()
		if err := database.CheckPlatformRole(ctx, pp); err != nil {
			return err
		}
		if plat, err = newPlatform(cfg, logger, pp); err != nil {
			return err
		}
	} else {
		logger.Warn("ORION_PLATFORM_DATABASE_URL is not set: the operator console (/admin) is disabled")
	}
	apiServer, err := api.New(api.Deps{Catalog: catalogSvc, Sync: syncSvc, Identity: ids, Entitlements: ents, Platform: plat, Tenancy: tenants, Logger: logger})
	if err != nil {
		return err
	}

	router := httpserver.NewRouter(httpserver.Options{
		Logger:     logger,
		UseSentry:  useSentry,
		TrustProxy: cfg.TrustProxy,
		Routes:     apiServer.Routes,
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

// keyrings reads the JWT signing keys. Outside local development they are required; locally,
// missing keys are replaced by random ones, so tokens stop working at every restart.
func keyrings(cfg config.Config, logger *slog.Logger) (tenant, device *identity.Keyring, err error) {
	return keyring(cfg, logger, "ORION_JWT_TENANT_KEYS", cfg.TenantJWTKeys, "ORION_JWT_DEVICE_KEYS", cfg.DeviceJWTKeys)
}

// operatorKeyring reads the operator session signing keys, with the same local-development rule.
func operatorKeyring(cfg config.Config, logger *slog.Logger) (*identity.Keyring, error) {
	k, _, err := keyring(cfg, logger, "ORION_JWT_OPERATOR_KEYS", cfg.OperatorJWTKeys, "", "x")
	return k, err
}

func keyring(cfg config.Config, logger *slog.Logger, nameA, specA, nameB, specB string) (a, b *identity.Keyring, err error) {
	load := func(name, spec string) (*identity.Keyring, error) {
		if spec != "" {
			k, err := identity.ParseKeyring(spec)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			return k, nil
		}
		if cfg.Env != "local" {
			return nil, fmt.Errorf("%s is required when ORION_ENV is %s", name, cfg.Env)
		}
		logger.Warn("using a random signing key; tokens will not survive a restart", "setting", name)
		return identity.NewEphemeralKeyring(), nil
	}
	if a, err = load(nameA, specA); err != nil {
		return nil, nil, err
	}
	if nameB != "" {
		if b, err = load(nameB, specB); err != nil {
			return nil, nil, err
		}
	}
	return a, b, nil
}

// worker runs river's job workers. Jobs span tenants, so it connects as orion_platform.
func worker(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	if cfg.PlatformDatabaseURL == "" {
		return errors.New("ORION_PLATFORM_DATABASE_URL is required")
	}
	if cfg.EmailProvider == "log" && cfg.Env == "production" {
		return errors.New("ORION_EMAIL_PROVIDER=log would write verification links to the log; choose a real provider for production")
	}
	pool, err := database.Connect(ctx, cfg.PlatformDatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := database.CheckPlatformRole(ctx, pool); err != nil {
		return err
	}

	workers := river.NewWorkers()
	idJobs := identity.NewJobs(identity.JobsDeps{
		Platform:  pool,
		Sender:    notify.LogSender{Logger: logger},
		Clock:     kernel.SystemClock{},
		Logger:    logger,
		PublicURL: cfg.PublicURL,
	})
	idJobs.AddWorkers(workers)

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Logger:       logger,
		Queues:       map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 10}},
		Workers:      workers,
		PeriodicJobs: idJobs.PeriodicJobs(),
	})
	if err != nil {
		return fmt.Errorf("river: %w", err)
	}
	if err := client.Start(ctx); err != nil {
		return fmt.Errorf("river: %w", err)
	}
	logger.Info("worker started")

	<-ctx.Done()
	logger.Info("worker stopping")
	// Stop lets running jobs finish; a second signal is not needed because the timeout bounds it.
	stopCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := client.Stop(stopCtx); err != nil {
		return fmt.Errorf("river stop: %w", err)
	}
	return nil
}

// newPlatform wires the operator module on a platform-role pool.
func newPlatform(cfg config.Config, logger *slog.Logger, pool *pgxpool.Pool) (*platform.Service, error) {
	if cfg.SecretsKey == "" {
		return nil, errors.New("ORION_SECRETS_KEY is required for the operator console (32 random bytes, base64url: openssl rand -base64 32 | tr '+/' '-_' | tr -d '=')")
	}
	box, err := kernel.NewBox(cfg.SecretsKey)
	if err != nil {
		return nil, fmt.Errorf("ORION_SECRETS_KEY: %w", err)
	}
	keys, err := operatorKeyring(cfg, logger)
	if err != nil {
		return nil, err
	}
	return platform.NewService(platform.Deps{
		Pool: pool, Box: box, Keys: keys, Clock: kernel.SystemClock{},
		Entitlements: entitlements.NewAdmin(pool), Tenants: tenancy.NewAdmin(pool),
	})
}
