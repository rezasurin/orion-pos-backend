package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestRouter(ready map[string]ReadinessCheck) http.Handler {
	return NewRouter(Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Ready: ready})
}

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestRouter(nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	ok := func(context.Context) error { return nil }
	failing := func(context.Context) error { return errors.New("database unreachable") }

	tests := []struct {
		name   string
		checks map[string]ReadinessCheck
		want   int
	}{
		{"all ok", map[string]ReadinessCheck{"database": ok, "migrations": ok}, http.StatusOK},
		{"one failing", map[string]ReadinessCheck{"database": failing, "migrations": ok}, http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			newTestRouter(tt.checks).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			var body struct{ Checks map[string]string }
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body.Checks) != len(tt.checks) {
				t.Errorf("checks = %v, want one entry per check", body.Checks)
			}
		})
	}
}

func TestPanicBecomes500(t *testing.T) {
	r := NewRouter(Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	r.Get("/boom", func(http.ResponseWriter, *http.Request) { panic("boom") })

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q", ct)
	}
}
