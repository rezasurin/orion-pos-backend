// Package config reads the service configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// Config is everything `orion` needs at startup.
type Config struct {
	Env string // local, staging or production

	// DatabaseURL is used by the running service. Its role must be a member of orion_app and
	// must not own the tables or bypass row-level security; `orion serve` refuses to start
	// otherwise.
	DatabaseURL string
	// MigrateDatabaseURL is used by `orion migrate`. Its role owns the schema.
	MigrateDatabaseURL string

	HTTPAddr        string
	ShutdownTimeout time.Duration

	LogLevel  slog.Level
	LogFormat string // json or text

	SentryDSN string // optional; errors are only logged when empty
}

// Load reads the configuration. Only the database URL needed by the command is required, so it
// is checked by the caller.
func Load() (Config, error) {
	c := Config{
		Env:                getenv("ORION_ENV", "local"),
		DatabaseURL:        os.Getenv("ORION_DATABASE_URL"),
		MigrateDatabaseURL: os.Getenv("ORION_MIGRATE_DATABASE_URL"),
		HTTPAddr:           getenv("ORION_HTTP_ADDR", ":8080"),
		LogFormat:          getenv("ORION_LOG_FORMAT", "json"),
		SentryDSN:          os.Getenv("ORION_SENTRY_DSN"),
	}

	var errs []error
	switch c.Env {
	case "local", "staging", "production":
	default:
		errs = append(errs, fmt.Errorf("ORION_ENV: unknown environment %q", c.Env))
	}
	if err := c.LogLevel.UnmarshalText([]byte(getenv("ORION_LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("ORION_LOG_LEVEL: %w", err))
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		errs = append(errs, fmt.Errorf("ORION_LOG_FORMAT: want json or text, got %q", c.LogFormat))
	}
	d, err := time.ParseDuration(getenv("ORION_SHUTDOWN_TIMEOUT", "15s"))
	if err != nil {
		errs = append(errs, fmt.Errorf("ORION_SHUTDOWN_TIMEOUT: %w", err))
	}
	c.ShutdownTimeout = d
	return c, errors.Join(errs...)
}

func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
