package serve

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRateLimit_Disabled(t *testing.T) {
	t.Parallel()
	mw := newRateLimitMiddleware(0, false)
	h := mw(http.HandlerFunc(okHandler))
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest(http.MethodGet, "/rpc", nil)
		req.RemoteAddr = "1.2.3.4:9999"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, rr.Code)
		}
	}
}

func TestRateLimit_BelowLimit(t *testing.T) {
	t.Parallel()
	// limit=5, burst=10; first 10 requests allowed (burst absorbs them)
	mw := newRateLimitMiddleware(5, false)
	h := mw(http.HandlerFunc(okHandler))
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/rpc", nil)
		req.RemoteAddr = "2.2.2.2:9999"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, rr.Code)
		}
	}
}

func TestRateLimit_ExceedsLimit(t *testing.T) {
	t.Parallel()
	// limit=1, burst=2; first 2 requests consume the burst, 3rd is rejected
	mw := newRateLimitMiddleware(1, false)
	h := mw(http.HandlerFunc(okHandler))

	statuses := make([]int, 3)
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/rpc", nil)
		req.RemoteAddr = "3.3.3.3:9999"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		statuses[i] = rr.Code
	}

	// First two succeed (burst=2), third must be 429
	if statuses[0] != http.StatusOK {
		t.Errorf("request 0: expected 200, got %d", statuses[0])
	}
	if statuses[1] != http.StatusOK {
		t.Errorf("request 1: expected 200, got %d", statuses[1])
	}
	if statuses[2] != http.StatusTooManyRequests {
		t.Errorf("request 2: expected 429, got %d", statuses[2])
	}
}

func TestRateLimit_BurstAllowed(t *testing.T) {
	t.Parallel()
	// limit=3, burst=6; all 6 burst requests allowed
	mw := newRateLimitMiddleware(3, false)
	h := mw(http.HandlerFunc(okHandler))
	for i := 0; i < 6; i++ {
		req := httptest.NewRequest(http.MethodGet, "/rpc", nil)
		req.RemoteAddr = "4.4.4.4:9999"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("burst request %d: expected 200, got %d", i, rr.Code)
		}
	}
	// 7th request exceeds burst
	req := httptest.NewRequest(http.MethodGet, "/rpc", nil)
	req.RemoteAddr = "4.4.4.4:9999"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("post-burst request: expected 429, got %d", rr.Code)
	}
}

func TestRateLimit_HealthExempt(t *testing.T) {
	t.Parallel()
	// limit=1, burst=2: /health should always pass regardless
	mw := newRateLimitMiddleware(1, false)
	h := mw(http.HandlerFunc(okHandler))
	for i := 0; i < 20; i++ {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		req.RemoteAddr = "5.5.5.5:9999"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("/health request %d: expected 200, got %d", i, rr.Code)
		}
	}
}

func TestRateLimit_PerIP(t *testing.T) {
	t.Parallel()
	// limit=1, burst=2: each IP gets its own bucket
	mw := newRateLimitMiddleware(1, false)
	h := mw(http.HandlerFunc(okHandler))

	// Exhaust IP A
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/rpc", nil)
		req.RemoteAddr = "10.0.0.1:9999"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
	}
	// IP A is now exhausted
	{
		req := httptest.NewRequest(http.MethodGet, "/rpc", nil)
		req.RemoteAddr = "10.0.0.1:9999"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusTooManyRequests {
			t.Errorf("IP A: expected 429, got %d", rr.Code)
		}
	}

	// IP B should still have its own fresh bucket
	{
		req := httptest.NewRequest(http.MethodGet, "/rpc", nil)
		req.RemoteAddr = "10.0.0.2:9999"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("IP B: expected 200, got %d", rr.Code)
		}
	}
}

func TestRateLimit_XFF_Trusted_WhenTrustEnabled(t *testing.T) {
	t.Parallel()
	// limit=1, burst=2: X-Forwarded-For should be used for IP extraction when trust is enabled
	mw := newRateLimitMiddleware(1, true)
	h := mw(http.HandlerFunc(okHandler))

	// Exhaust the XFF IP
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/rpc", nil)
		req.RemoteAddr = "192.168.1.1:9999" // proxy IP
		req.Header.Set("X-Forwarded-For", "203.0.113.5, 192.168.1.1")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
	}
	// Next request from same XFF IP (even different RemoteAddr) should be rejected
	req := httptest.NewRequest(http.MethodGet, "/rpc", nil)
	req.RemoteAddr = "192.168.1.2:9999" // different proxy IP
	req.Header.Set("X-Forwarded-For", "203.0.113.5")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("XFF exhausted: expected 429, got %d", rr.Code)
	}

	// A different XFF IP should succeed
	req2 := httptest.NewRequest(http.MethodGet, "/rpc", nil)
	req2.RemoteAddr = "192.168.1.1:9999"
	req2.Header.Set("X-Forwarded-For", "203.0.113.99")
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Errorf("different XFF IP: expected 200, got %d", rr2.Code)
	}
}

func TestRateLimit_XFF_Ignored_WhenTrustDisabled(t *testing.T) {
	t.Parallel()
	// limit=1, burst=2: XFF should be ignored, rate limiting uses RemoteAddr
	mw := newRateLimitMiddleware(1, false)
	h := mw(http.HandlerFunc(okHandler))

	// Two requests from the same RemoteAddr but different XFF IPs should share the same bucket.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/rpc", nil)
		req.RemoteAddr = "10.0.0.1:9999"
		req.Header.Set("X-Forwarded-For", "1.2.3.4") // forged IP
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
	}
	// Third request from same RemoteAddr should be rate-limited (bucket exhausted by RemoteAddr)
	req := httptest.NewRequest(http.MethodGet, "/rpc", nil)
	req.RemoteAddr = "10.0.0.1:9999"
	req.Header.Set("X-Forwarded-For", "9.9.9.9") // different forged IP — must still be rate-limited
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 when XFF ignored and RemoteAddr exhausted, got %d", rr.Code)
	}

	// A genuinely different RemoteAddr should not be affected.
	req2 := httptest.NewRequest(http.MethodGet, "/rpc", nil)
	req2.RemoteAddr = "10.0.0.2:9999"
	req2.Header.Set("X-Forwarded-For", "1.2.3.4") // same forged IP — irrelevant when trust disabled
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Errorf("different RemoteAddr: expected 200, got %d", rr2.Code)
	}
}
