package httpserver

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLimiterBurstThenRefill(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := NewLimiter(10*time.Second, 3)
	l.now = func() time.Time { return now }

	for i := range 3 {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("request %d of the burst was refused", i+1)
		}
	}
	ok, retry := l.Allow("k")
	if ok || retry <= 0 || retry > 10*time.Second {
		t.Fatalf("4th request: ok=%v retry=%v, want refused with a wait of up to 10s", ok, retry)
	}
	if ok, _ := l.Allow("other"); !ok {
		t.Error("another key shares the bucket")
	}

	now = now.Add(10 * time.Second)
	if ok, _ := l.Allow("k"); !ok {
		t.Error("a token should have been earned back after 10s")
	}
	if ok, _ := l.Allow("k"); ok {
		t.Error("only one token should have been earned back")
	}

	// A long rest refills to the burst, not beyond it.
	now = now.Add(time.Hour)
	for range 3 {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatal("burst not restored after a long rest")
		}
	}
	if ok, _ := l.Allow("k"); ok {
		t.Error("refilled beyond the burst")
	}
}

func TestLimiterForgetsIdleKeys(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := NewLimiter(time.Second, 2)
	l.now = func() time.Time { return now }
	for _, k := range []string{"a", "b", "c"} {
		l.Allow(k)
	}
	now = now.Add(sweepEvery + time.Minute)
	l.Allow("d")
	if len(l.buckets) != 1 {
		t.Errorf("buckets = %d after a sweep, want only the active key", len(l.buckets))
	}
}

func TestClientIP(t *testing.T) {
	get := func(trust bool, remote, xff string) string {
		var got string
		h := clientIPMiddleware(trust)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = ClientIP(r.Context()) }))
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = remote
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		return got
	}
	for name, tt := range map[string]struct {
		trust       bool
		remote, xff string
		want        string
	}{
		"direct":                   {false, "203.0.113.7:5000", "", "203.0.113.7"},
		"header ignored":           {false, "203.0.113.7:5000", "1.2.3.4", "203.0.113.7"},
		"proxy appends":            {true, "10.0.0.1:5000", "6.6.6.6, 198.51.100.9", "198.51.100.9"},
		"forged prefix is ignored": {true, "10.0.0.1:5000", "1.2.3.4, 198.51.100.9", "198.51.100.9"},
		"junk header":              {true, "10.0.0.1:5000", "not-an-ip", "10.0.0.1"},
		"no header behind proxy":   {true, "10.0.0.1:5000", "", "10.0.0.1"},
		"ipv6":                     {false, "[2001:db8::1]:5000", "", "2001:db8::1"},
	} {
		if got := get(tt.trust, tt.remote, tt.xff); got != tt.want {
			t.Errorf("%s: ClientIP = %q, want %q", name, got, tt.want)
		}
	}
}

func TestBodyIsLimited(t *testing.T) {
	r := newTestRouter(nil)
	r.(interface {
		Post(string, http.HandlerFunc)
	}).Post("/echo", func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 2<<20)
		n, err := r.Body.Read(buf)
		for err == nil {
			var m int
			m, err = r.Body.Read(buf[n:])
			n += m
		}
		if err != nil && n >= maxBodyBytes {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	big := make([]byte, 2<<20)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/echo", bytesReader(big)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
