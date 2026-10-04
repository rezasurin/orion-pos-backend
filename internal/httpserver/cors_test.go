package httpserver

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// corsRouter has one route that needs a token, like most of the API.
func corsRouter(origins ...string) http.Handler {
	return NewRouter(Options{
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		CORSAllowedOrigins: origins,
		Routes: func(r chi.Router) {
			r.Post("/v1/thing", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(http.StatusUnauthorized)
			})
		},
	})
}

func do(h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(""))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCORSAnswersAPreflightWithoutCredentials(t *testing.T) {
	h := corsRouter("https://app.orion.test")
	rec := do(h, http.MethodOptions, "/v1/thing", map[string]string{
		"Origin":                         "https://app.orion.test",
		"Access-Control-Request-Method":  "POST",
		"Access-Control-Request-Headers": "authorization,content-type",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204 (it must not reach authentication)", rec.Code)
	}
	for k, want := range map[string]string{
		"Access-Control-Allow-Origin":  "https://app.orion.test",
		"Access-Control-Allow-Methods": "GET, POST, PUT, PATCH, DELETE",
		"Access-Control-Allow-Headers": "Authorization, Content-Type",
		"Access-Control-Max-Age":       "600",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Error("credentials must never be allowed: the API uses bearer tokens, not cookies")
	}
	if v := strings.Join(rec.Header().Values("Vary"), ","); !strings.Contains(v, "Origin") {
		t.Errorf("Vary = %q, want it to include Origin", v)
	}
}

func TestCORSLetsTheAppReadResponsesAndErrors(t *testing.T) {
	h := corsRouter("https://app.orion.test")
	rec := do(h, http.MethodPost, "/v1/thing", map[string]string{"Origin": "https://app.orion.test"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d: a real request must still reach the handler", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.orion.test" {
		t.Errorf("Allow-Origin = %q on an error response: the app could not read it", got)
	}
	if got := rec.Header().Get("Access-Control-Expose-Headers"); got != "Retry-After" {
		t.Errorf("Expose-Headers = %q, want Retry-After so the app can back off", got)
	}
}

func TestCORSMatchesOriginsExactly(t *testing.T) {
	h := corsRouter("https://app.orion.test")
	for _, origin := range []string{
		"https://evil.test",
		"http://app.orion.test",         // other scheme
		"https://app.orion.test:8443",   // other port
		"https://app.orion.test.evil.x", // a prefix is not a match
		"https://sub.app.orion.test",
		"null",
	} {
		rec := do(h, http.MethodPost, "/v1/thing", map[string]string{"Origin": origin})
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("origin %q was allowed (%q)", origin, got)
		}
		pre := do(h, http.MethodOptions, "/v1/thing", map[string]string{"Origin": origin, "Access-Control-Request-Method": "POST"})
		if pre.Header().Get("Access-Control-Allow-Origin") != "" || pre.Code == http.StatusNoContent {
			t.Errorf("preflight from %q was answered: %d %v", origin, pre.Code, pre.Header())
		}
		if v := strings.Join(rec.Header().Values("Vary"), ","); !strings.Contains(v, "Origin") {
			t.Errorf("origin %q: Vary = %q; a cached refusal must not be served to another origin", origin, v)
		}
	}
}

func TestCORSIsOffWithoutOrigins(t *testing.T) {
	h := corsRouter()
	for _, hdr := range []map[string]string{
		{"Origin": "https://app.orion.test"},
		{"Origin": "https://app.orion.test", "Access-Control-Request-Method": "POST"},
	} {
		rec := do(h, http.MethodOptions, "/v1/thing", hdr)
		for k := range rec.Header() {
			if strings.HasPrefix(k, "Access-Control-") {
				t.Errorf("%s set although no origin is configured", k)
			}
		}
	}
}

func TestCORSLeavesRequestsWithoutAnOriginAlone(t *testing.T) {
	h := corsRouter("https://app.orion.test")
	rec := do(h, http.MethodPost, "/v1/thing", nil)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Vary") != "" {
		t.Errorf("a same-origin or non-browser request changed: %d %v", rec.Code, rec.Header())
	}
}
