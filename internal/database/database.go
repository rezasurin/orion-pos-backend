// Package database opens connection pools, runs migrations and reports the schema version.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/rezasurin/orion-pos-backend/migrations"
)

// Connect opens a pool and checks the database is reachable.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	if url == "" {
		return nil, errors.New("database: empty connection URL")
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("database: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: ping: %w", err)
	}
	return pool, nil
}

// CheckAppRole refuses a connection whose role would silently bypass row-level security: a
// superuser, a role with BYPASSRLS, or the owner of the tables. It also requires membership of
// orion_app, which the policies are written for.
func CheckAppRole(ctx context.Context, pool *pgxpool.Pool) error {
	return checkRole(ctx, pool, "orion_app")
}

// CheckPlatformRole is CheckAppRole for the worker's connection: a member of orion_platform that
// is not a superuser, does not bypass row-level security and does not own the tables.
func CheckPlatformRole(ctx context.Context, pool *pgxpool.Pool) error {
	return checkRole(ctx, pool, "orion_platform")
}

func checkRole(ctx context.Context, pool *pgxpool.Pool, group string) error {
	var (
		user                 string
		super, bypass, isMem bool
		ownsTenant           bool
	)
	err := pool.QueryRow(ctx, `
		SELECT current_user,
		       r.rolsuper,
		       r.rolbypassrls,
		       pg_has_role(current_user, $1, 'USAGE'),
		       EXISTS (SELECT 1 FROM pg_tables
		               WHERE schemaname = 'public' AND tablename = 'tenant' AND tableowner = current_user)
		FROM pg_roles r WHERE r.rolname = current_user`, group,
	).Scan(&user, &super, &bypass, &isMem, &ownsTenant)
	if err != nil {
		return fmt.Errorf("database: check role: %w", err)
	}
	switch {
	case super:
		return fmt.Errorf("database: role %q is a superuser, which bypasses row-level security", user)
	case bypass:
		return fmt.Errorf("database: role %q has BYPASSRLS", user)
	case ownsTenant:
		return fmt.Errorf("database: role %q owns the tables, which bypasses row-level security", user)
	case !isMem:
		return fmt.Errorf("database: role %q is not a member of %s", user, group)
	}
	return nil
}

func newProvider(db *sql.DB) (*goose.Provider, error) {
	return goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
}

// Migrate applies every pending migration using the schema owner's connection URL.
func Migrate(ctx context.Context, url string, logger *slog.Logger) error {
	return withProvider(ctx, url, func(p *goose.Provider) error {
		results, err := p.Up(ctx)
		for _, r := range results {
			logger.Info("migration applied", "migration", r.Source.Version, "file", r.Source.Path, "duration", r.Duration)
		}
		return err
	})
}

// MigrateDown rolls back the most recent migration.
func MigrateDown(ctx context.Context, url string, logger *slog.Logger) error {
	return withProvider(ctx, url, func(p *goose.Provider) error {
		r, err := p.Down(ctx)
		if r != nil {
			logger.Info("migration rolled back", "migration", r.Source.Version, "file", r.Source.Path)
		}
		return err
	})
}

// MigrationStatus logs whether each migration is applied.
func MigrationStatus(ctx context.Context, url string, logger *slog.Logger) error {
	return withProvider(ctx, url, func(p *goose.Provider) error {
		statuses, err := p.Status(ctx)
		for _, s := range statuses {
			logger.Info("migration", "migration", s.Source.Version, "file", s.Source.Path, "state", s.State, "applied_at", s.AppliedAt)
		}
		return err
	})
}

func withProvider(ctx context.Context, url string, fn func(*goose.Provider) error) error {
	pool, err := Connect(ctx, url)
	if err != nil {
		return err
	}
	db := stdlib.OpenDBFromPool(pool)
	defer func() {
		_ = db.Close()
		pool.Close()
	}()
	p, err := newProvider(db)
	if err != nil {
		return fmt.Errorf("database: migrations: %w", err)
	}
	return fn(p)
}

// ExpectedVersion is the newest migration embedded in this binary.
func ExpectedVersion() (int64, error) {
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return 0, err
	}
	var latest int64
	for _, name := range names {
		v, err := goose.NumericComponent(name)
		if err != nil {
			return 0, fmt.Errorf("database: migration %s: %w", name, err)
		}
		latest = max(latest, v)
	}
	return latest, nil
}

// AppliedVersion is the newest migration applied to the database the pool points at.
func AppliedVersion(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var v int64
	err := pool.QueryRow(ctx,
		`SELECT coalesce(max(version_id), 0) FROM goose_db_version WHERE is_applied`).Scan(&v)
	return v, err
}
