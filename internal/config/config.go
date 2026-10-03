// Package config reads the service configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
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
	// PlatformDatabaseURL is used by `orion worker`, whose jobs span tenants. Its role must be a
	// member of orion_platform, with the same restrictions as DatabaseURL.
	PlatformDatabaseURL string

	// PublicURL is the front end's base URL, used for links in emails.
	PublicURL string
	// TenantJWTKeys and DeviceJWTKeys are "kid:base64url-secret[,kid:secret...]". The first key
	// signs; the rest only verify, which is how a key is rotated. Required outside local.
	TenantJWTKeys string
	DeviceJWTKeys string
	// OperatorJWTKeys signs operator sessions, with its own keys so a tenant key never signs one.
	OperatorJWTKeys string
	// SecretsKey is a base64url 32-byte key that encrypts operator TOTP seeds at rest. Losing it
	// locks every operator out; keep it in the secret store, separate from the database backups.
	SecretsKey string

	// EmailProvider names how email is sent. Only "log" exists so far, which writes messages to
	// the log and is refused in production.
	EmailProvider string
	// TrustProxy says a proxy in front of the service appends the caller's address to
	// X-Forwarded-For. Enable it only behind exactly one such proxy.
	TrustProxy bool

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
		Env:                 getenv("ORION_ENV", "local"),
		DatabaseURL:         os.Getenv("ORION_DATABASE_URL"),
		MigrateDatabaseURL:  os.Getenv("ORION_MIGRATE_DATABASE_URL"),
		PlatformDatabaseURL: os.Getenv("ORION_PLATFORM_DATABASE_URL"),
		PublicURL:           getenv("ORION_PUBLIC_URL", "http://localhost:5173"),
		TenantJWTKeys:       os.Getenv("ORION_JWT_TENANT_KEYS"),
		DeviceJWTKeys:       os.Getenv("ORION_JWT_DEVICE_KEYS"),
		OperatorJWTKeys:     os.Getenv("ORION_JWT_OPERATOR_KEYS"),
		SecretsKey:          os.Getenv("ORION_SECRETS_KEY"),
		EmailProvider:       getenv("ORION_EMAIL_PROVIDER", "log"),
		HTTPAddr:            getenv("ORION_HTTP_ADDR", ":8080"),
		LogFormat:           getenv("ORION_LOG_FORMAT", "json"),
		SentryDSN:           os.Getenv("ORION_SENTRY_DSN"),
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

	tp, err := parseBool("ORION_TRUST_PROXY")
	if err != nil {
		errs = append(errs, err)
	}
	c.TrustProxy = tp

	switch c.EmailProvider {
	case "log":
	default:
		errs = append(errs, fmt.Errorf("ORION_EMAIL_PROVIDER: unknown provider %q (want log)", c.EmailProvider))
	}
	return c, errors.Join(errs...)
}

func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func parseBool(key string) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: want true or false, got %q", key, v)
	}
	return b, nil
}
