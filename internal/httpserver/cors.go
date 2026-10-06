package httpserver

import (
	"net/http"
	"strings"
)

// cors lets browsers on the listed origins call the API.
//
// The API authenticates with bearer tokens in the Authorization header and never with cookies, so
// credentials are never allowed and a request from an unlisted origin is not a security hole in
// itself: the list decides which web apps a browser lets read responses. Origins are matched
// exactly (scheme, host and port); there is no wildcard.
//
// A preflight (OPTIONS with Access-Control-Request-Method) is answered here, before routing and
// authentication, because browsers send it without credentials. Everything else passes through
// with the headers added, so errors are readable by the app too.
func cors(allowed []string) func(http.Handler) http.Handler {
	set := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		set[o] = struct{}{}
	}
	if len(set) == 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			// The answer depends on the Origin, so caches must not share it between origins.
			w.Header().Add("Vary", "Origin")
			if _, ok := set[strings.ToLower(origin)]; !ok {
				next.ServeHTTP(w, r)
				return
			}
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Add("Vary", "Access-Control-Request-Method")
				h.Add("Vary", "Access-Control-Request-Headers")
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			// Without this a browser app cannot read Retry-After from a 429.
			h.Set("Access-Control-Expose-Headers", "Retry-After")
			next.ServeHTTP(w, r)
		})
	}
}
