package httpserver

import (
	"sync"
	"time"
)

// Limiter is an in-process token bucket per key, enough while one API instance sees every
// request (BACKEND_PLAN.md sections 4.3 and 4.13). With more instances the limits multiply, which
// is one of the reasons to move them to Redis.
type Limiter struct {
	every time.Duration // time to earn one token back
	burst float64
	now   func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

const (
	sweepEvery = 5 * time.Minute
	sweepAbove = 10_000
	maxBuckets = 200_000 // a flood of distinct keys cannot grow the map without bound
)

// NewLimiter allows `burst` requests at once per key and one more every `every`.
func NewLimiter(every time.Duration, burst int) *Limiter {
	return &Limiter{every: every, burst: float64(burst), now: time.Now, buckets: map[string]*bucket{}}
}

// Allow takes one token for key. When none is left it returns false and how long until one is.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)

	b, found := l.buckets[key]
	if !found {
		if len(l.buckets) >= maxBuckets {
			// Under a flood of fresh keys, refuse new ones instead of growing.
			return false, l.every
		}
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+float64(now.Sub(b.at))/float64(l.every))
	b.at = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) * float64(l.every))
	}
	b.tokens--
	return true, 0
}

// sweep drops buckets that have refilled completely: forgetting them changes nothing.
func (l *Limiter) sweep(now time.Time) {
	if len(l.buckets) < sweepAbove && now.Sub(l.lastSweep) < sweepEvery {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if b.tokens+float64(now.Sub(b.at))/float64(l.every) >= l.burst {
			delete(l.buckets, k)
		}
	}
}
