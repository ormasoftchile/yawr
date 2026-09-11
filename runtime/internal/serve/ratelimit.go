package serve

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// newRateLimitMiddleware returns middleware that rate-limits requests per IP.
// limit=0 disables rate limiting. The limiter uses a token bucket with
// burst size of 2*limit to allow brief spikes.
func newRateLimitMiddleware(limit int, trustProxyHeaders bool) middleware {
	if limit <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}

	limiters := &ipLimiters{
		m:       make(map[string]*rateLimiterEntry),
		limit:   rate.Limit(limit),
		burst:   limit * 2,
		maxSize: 10000,
	}
	go limiters.cleanupLoop()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				next.ServeHTTP(w, r)
				return
			}
			ip := extractIP(r, trustProxyHeaders)
			if !limiters.Allow(ip) {
				http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type ipLimiters struct {
	mu      sync.Mutex
	m       map[string]*rateLimiterEntry
	limit   rate.Limit
	burst   int
	maxSize int
}

type rateLimiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func (l *ipLimiters) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, ok := l.m[ip]
	if !ok {
		if len(l.m) >= l.maxSize {
			l.evictOldest()
		}
		entry = &rateLimiterEntry{
			limiter: rate.NewLimiter(l.limit, l.burst),
		}
		l.m[ip] = entry
	}
	entry.lastSeen = time.Now()
	return entry.limiter.Allow()
}

func (l *ipLimiters) cleanupLoop() {
	ticker := time.NewTicker(60 * time.Second)
	for range ticker.C {
		l.mu.Lock()
		cutoff := time.Now().Add(-5 * time.Minute)
		for ip, entry := range l.m {
			if entry.lastSeen.Before(cutoff) {
				delete(l.m, ip)
			}
		}
		l.mu.Unlock()
	}
}

func (l *ipLimiters) evictOldest() {
	var oldestIP string
	var oldestTime time.Time
	for ip, entry := range l.m {
		if oldestTime.IsZero() || entry.lastSeen.Before(oldestTime) {
			oldestIP = ip
			oldestTime = entry.lastSeen
		}
	}
	if oldestIP != "" {
		delete(l.m, oldestIP)
	}
}

func extractIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if ip := strings.Split(xff, ",")[0]; ip != "" {
				return strings.TrimSpace(ip)
			}
		}
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}
