// Package httpserver builds the HTTP router: shared middleware and the health endpoints. Module
// routes are mounted here as they arrive (generated from the OpenAPI document from B0.3 on).
package httpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// ReadinessCheck reports whether a dependency is ready to serve traffic.
type ReadinessCheck func(ctx context.Context) error

// Options configures the router.
type Options struct {
	Logger    *slog.Logger
	Ready     map[string]ReadinessCheck
	UseSentry bool
}

// NewRouter returns the service's root router. Module routes are mounted on it.
func NewRouter(o Options) chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestLogger(o.Logger))
	if o.UseSentry {
		r.Use(sentryhttp.New(sentryhttp.Options{Repanic: true}).Handle)
	}
	r.Use(recoverer(o.Logger))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", readyz(o.Ready))
	return r
}

func readyz(checks map[string]ReadinessCheck) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		status, results := http.StatusOK, map[string]string{}
		for name, check := range checks {
			if err := check(ctx); err != nil {
				status = http.StatusServiceUnavailable
				results[name] = err.Error()
				continue
			}
			results[name] = "ok"
		}
		writeJSON(w, status, map[string]any{"checks": results})
	}
}

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			// Health probes are frequent and uninteresting unless they fail.
			if (r.URL.Path == "/healthz" || r.URL.Path == "/readyz") && ww.Status() < 400 {
				return
			}
			logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
				slog.String("request_id", middleware.GetReqID(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("duration", time.Since(start)),
			)
		})
	}
}

// recoverer turns a panic into a 500 and a log line. When Sentry is on, its middleware runs
// first and re-panics after reporting, so the panic still ends here.
func recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if rec == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
					panic(rec)
				}
				logger.ErrorContext(r.Context(), "panic",
					"request_id", middleware.GetReqID(r.Context()),
					"panic", rec,
				)
				writeJSON(w, http.StatusInternalServerError, map[string]string{
					"type":  "about:blank",
					"title": "Internal Server Error",
					"code":  "internal",
				})
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	ct := "application/json"
	if status >= 500 {
		ct = "application/problem+json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// InitSentry configures error reporting. It returns a flush function to call on shutdown.
func InitSentry(dsn, env, release string) (flush func(), err error) {
	if err := sentry.Init(sentry.ClientOptions{Dsn: dsn, Environment: env, Release: release}); err != nil {
		return nil, err
	}
	return func() { sentry.Flush(2 * time.Second) }, nil
}
