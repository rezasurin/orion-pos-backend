// Package config reads the service configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
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
	// CORSAllowedOrigins are the browser origins (scheme://host[:port]) allowed to call the API
	// from another origin. Empty means none: same-origin only. In local development it defaults to
	// the origin of PublicURL.
	CORSAllowedOrigins []string
	// TenantJWTKeys and DeviceJWTKeys are "kid:base64url-secret[,kid:secret...]". The first key
	// signs; the rest only verify, which is how a key is rotated. Required outside local.
	TenantJWTKeys string
	DeviceJWTKeys string
	// OperatorJWTKeys signs operator sessions, with its own keys so a tenant key never signs one.
	OperatorJWTKeys string
	// SecretsKey is a base64url 32-byte key that encrypts operator TOTP seeds at rest. Losing it
	// locks every operator out; keep it in the secret store, separate from the database backups.
	SecretsKey string

	// EmailProvider names how email is sent: "log" writes messages to the log and is refused in
	// production; "resend" sends through Resend with EmailAPIKey, from EmailFrom.
	EmailProvider string
	EmailAPIKey   string
	EmailFrom     string
	// AlertUnsyncedAfter is how old the oldest event a device still holds may be before the owner is
	// emailed; AlertSilentAfter how long a device with an open shift may go unheard. Zero takes the
	// defaults (30 minutes and 3 hours). AlertOperatorEmail, if set, gets a copy of every alert.
	AlertUnsyncedAfter time.Duration
	AlertSilentAfter   time.Duration
	AlertOperatorEmail string
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
		EmailAPIKey:         os.Getenv("ORION_EMAIL_API_KEY"),
		EmailFrom:           os.Getenv("ORION_EMAIL_FROM"),
		HTTPAddr:            getenv("ORION_HTTP_ADDR", ":8080"),
		LogFormat:           getenv("ORION_LOG_FORMAT", "json"),
		SentryDSN:           os.Getenv("ORION_SENTRY_DSN"),
		AlertOperatorEmail:  os.Getenv("ORION_ALERT_OPERATOR_EMAIL"),
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
	for _, v := range []struct {
		key string
		dst *time.Duration
	}{{"ORION_ALERT_UNSYNCED_AFTER", &c.AlertUnsyncedAfter}, {"ORION_ALERT_SILENT_AFTER", &c.AlertSilentAfter}} {
		if raw := os.Getenv(v.key); raw != "" {
			dur, err := time.ParseDuration(raw)
			if err != nil || dur <= 0 {
				errs = append(errs, fmt.Errorf("%s: want a positive duration like 30m, got %q", v.key, raw))
				continue
			}
			*v.dst = dur
		}
	}

	origins, err := parseOrigins(os.Getenv("ORION_CORS_ALLOWED_ORIGINS"))
	if err != nil {
		errs = append(errs, fmt.Errorf("ORION_CORS_ALLOWED_ORIGINS: %w", err))
	}
	if len(origins) == 0 && os.Getenv("ORION_CORS_ALLOWED_ORIGINS") == "" && c.Env == "local" {
		if o, err := parseOrigins(c.PublicURL); err == nil {
			origins = o
		}
	}
	c.CORSAllowedOrigins = origins

	tp, err := parseBool("ORION_TRUST_PROXY")
	if err != nil {
		errs = append(errs, err)
	}
	c.TrustProxy = tp

	switch c.EmailProvider {
	case "log":
	case "resend":
		if c.EmailAPIKey == "" {
			errs = append(errs, errors.New("ORION_EMAIL_API_KEY: required with ORION_EMAIL_PROVIDER=resend"))
		}
		if c.EmailFrom == "" {
			errs = append(errs, errors.New("ORION_EMAIL_FROM: required with ORION_EMAIL_PROVIDER=resend"))
		}
	default:
		errs = append(errs, fmt.Errorf("ORION_EMAIL_PROVIDER: unknown provider %q (want log or resend)", c.EmailProvider))
	}
	return c, errors.Join(errs...)
}

// parseOrigins reads a comma-separated list of origins. Each must be exactly scheme://host[:port]
// (a trailing slash is tolerated): a path, a query, credentials or a wildcard is a mistake that
// would otherwise silently match nothing, or too much.
func parseOrigins(raw string) ([]string, error) {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if strings.Contains(item, "*") {
			return nil, fmt.Errorf("%q: wildcards are not allowed, list each origin", item)
		}
		u, err := url.Parse(strings.TrimSuffix(item, "/"))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("%q: want an origin like https://app.example.com (scheme and host, no path)", item)
		}
		out = append(out, strings.ToLower(u.Scheme+"://"+u.Host))
	}
	return out, nil
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
