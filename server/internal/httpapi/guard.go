package httpapi

import (
	"context"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

const requestTimeout = 10 * time.Second

// REST handlers pass this deadline to the database and object storage. Server
// WriteTimeout alone only limits socket writes; it does not cancel database work.
func restHandler(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		next(w, r.WithContext(ctx))
	}
}

type bucket struct {
	tokens float64
	at     time.Time
}

// This bounded, process-local limiter is a fallback, not a distributed quota.
// Do not trust forwarding headers from arbitrary clients. A reverse proxy must
// enforce its own per-client limits before forwarding requests to this API.
type attemptLimiter struct {
	mu          sync.Mutex
	buckets     map[netip.Addr]bucket
	burst       float64
	interval    time.Duration
	maxEntries  int
	nextCleanup time.Time
}

func newAttemptLimiter(burst int, interval time.Duration) *attemptLimiter {
	return &attemptLimiter{
		buckets: make(map[netip.Addr]bucket), burst: float64(burst),
		interval: interval, maxEntries: 4096,
	}
}

func (l *attemptLimiter) allow(address netip.Addr, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	retention := time.Duration(l.burst) * l.interval
	if !now.Before(l.nextCleanup) {
		for key, entry := range l.buckets {
			if now.Sub(entry.at) >= retention {
				delete(l.buckets, key)
			}
		}
		l.nextCleanup = now.Add(retention)
	}
	entry, exists := l.buckets[address]
	if !exists {
		if len(l.buckets) >= l.maxEntries {
			return false
		}
		entry = bucket{tokens: l.burst, at: now}
	}
	entry.tokens = min(l.burst, entry.tokens+now.Sub(entry.at).Seconds()/l.interval.Seconds())
	entry.at = now
	allowed := entry.tokens >= 1
	if allowed {
		entry.tokens--
	}
	l.buckets[address] = entry
	return allowed
}

func (l *attemptLimiter) wrap(next http.HandlerFunc) http.HandlerFunc {
	return l.wrapShared(nil, "", next)
}

func (l *attemptLimiter) wrapShared(shared RateLimiter, scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		peer, err := netip.ParseAddrPort(r.RemoteAddr)
		allowed := false
		if err == nil {
			address := peer.Addr().Unmap()
			if shared != nil {
				ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
				allowed, err = shared.Allow(ctx, scope, address, int(l.burst), l.interval)
				cancel()
			}
			if shared == nil || err != nil {
				allowed = l.allow(address, time.Now())
			}
		}
		if !allowed {
			// Covers both an exhausted bucket and a full limiter table.
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "rate_limited")
			return
		}
		next(w, r)
	}
}
