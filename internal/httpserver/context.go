package httpserver

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
)

type ctxKey int

const (
	clientIPKey ctxKey = iota
	logAttrsKey
)

// ClientIP is the address of the caller as the service sees it. See the trustProxy argument of
// clientIPMiddleware for what that means behind a proxy.
func ClientIP(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey).(string)
	return ip
}

// clientIPMiddleware records the caller's address. Behind exactly one trusted proxy (a load
// balancer that appends to X-Forwarded-For) pass trustProxy: the last entry is then the address
// that proxy saw. Without a proxy, never trust the header: any client could forge it.
func clientIPMiddleware(trustProxy bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				ip = r.RemoteAddr
			}
			if trustProxy {
				if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
					parts := strings.Split(xff, ",")
					if last := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(last) != nil {
						ip = last
					}
				}
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPKey, ip)))
		})
	}
}

// logAttrs collects attributes that later middleware learns (tenant, principal) so the request
// log line, written when the request ends, can include them.
type logAttrs struct {
	mu    sync.Mutex
	attrs []slog.Attr
}

func withLogAttrs(ctx context.Context) (context.Context, *logAttrs) {
	la := &logAttrs{}
	return context.WithValue(ctx, logAttrsKey, la), la
}

func (la *logAttrs) snapshot() []slog.Attr {
	la.mu.Lock()
	defer la.mu.Unlock()
	return append([]slog.Attr(nil), la.attrs...)
}

// AddLogAttrs adds attributes to the access log line of the current request. It does nothing
// outside a request. Never add secrets or personal data.
func AddLogAttrs(ctx context.Context, attrs ...slog.Attr) {
	la, ok := ctx.Value(logAttrsKey).(*logAttrs)
	if !ok {
		return
	}
	la.mu.Lock()
	defer la.mu.Unlock()
	la.attrs = append(la.attrs, attrs...)
}
