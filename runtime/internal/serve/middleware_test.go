package serve

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func okHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// makeUnsignedJWT creates a minimal JWT with alg:none and an empty signature.
// Useful only for testing rejection of unsupported algorithms.
func makeUnsignedJWT(exp, iat int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"iat":%d}`, exp, iat)))
	return header + "." + payload + "."
}

// makeSignedJWT creates a proper HS256-signed JWT for testing.
func makeSignedJWT(t *testing.T, secret []byte, exp, iat int64) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"iat":%d}`, exp, iat)))
	signingInput := header + "." + payload
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return header + "." + payload + "." + sig
}

func TestCORSMiddleware_AllowedOrigin(t *testing.T) {
	mw := newCORSMiddleware([]string{"https://example.com"})
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/rpc", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Fatalf("expected CORS origin header, got %q", got)
	}
}

func TestCORSMiddleware_Preflight(t *testing.T) {
	mw := newCORSMiddleware(nil)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodOptions, "/rpc", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Fatalf("expected CORS origin header on preflight, got %q", got)
	}
}

func TestCORSMiddleware_NoOrigin(t *testing.T) {
	mw := newCORSMiddleware(nil)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/rpc", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("expected no CORS headers when no Origin, got %q", got)
	}
}

func TestCORSMiddleware_DisallowedOrigin(t *testing.T) {
	mw := newCORSMiddleware([]string{"https://allowed.com"})
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/rpc", nil)
	req.Header.Set("Origin", "https://other.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("expected no CORS headers for disallowed origin, got %q", got)
	}
}

func TestBearerAuth_ValidToken(t *testing.T) {
	mw := newBearerAuthMiddleware("secret", 0, nil)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodPost, "/rpc", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid token, got %d", rec.Code)
	}
}

func TestBearerAuth_InvalidToken(t *testing.T) {
	mw := newBearerAuthMiddleware("secret", 0, nil)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodPost, "/rpc", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 with wrong token, got %d", rec.Code)
	}
}

func TestBearerAuth_MissingToken(t *testing.T) {
	mw := newBearerAuthMiddleware("secret", 0, nil)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodPost, "/rpc", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with missing Authorization, got %d", rec.Code)
	}
}

func TestBearerAuth_NoConfiguredToken(t *testing.T) {
	mw := newBearerAuthMiddleware("", 0, nil)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodPost, "/rpc", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when no token configured, got %d", rec.Code)
	}
}

func TestBearerAuth_HealthExempt(t *testing.T) {
	mw := newBearerAuthMiddleware("secret", 0, nil)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected /health to be exempt from auth, got %d", rec.Code)
	}
}

// TestBearerAuth_UsesConstantTimeCompare verifies behavioral correctness of the
// constant-time comparison path for various prefix-match scenarios.
func TestBearerAuth_UsesConstantTimeCompare(t *testing.T) {
	mw := newBearerAuthMiddleware("secrettoken123", 0, nil)
	handler := mw(http.HandlerFunc(okHandler))

	cases := []struct {
		name   string
		token  string
		status int
	}{
		{"correct", "secrettoken123", http.StatusOK},
		{"wrong_first_byte", "xecrettoken123", http.StatusForbidden},
		{"wrong_last_byte", "secrettoken12x", http.StatusForbidden},
		{"wrong_length", "secret", http.StatusForbidden},
		{"empty_no_header", "", http.StatusUnauthorized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/rpc", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Errorf("got %d, want %d", rec.Code, tc.status)
			}
		})
	}
}

// TestBearerAuth_JWTValid verifies that a properly signed HS256 JWT with a future exp is accepted.
func TestBearerAuth_JWTValid(t *testing.T) {
	secret := []byte("a-32-byte-hmac-secret-for-testing!!")
	now := time.Now()
	jwt := makeSignedJWT(t, secret, now.Add(1*time.Hour).Unix(), now.Add(-1*time.Minute).Unix())
	mw := newBearerAuthMiddleware("", 24*time.Hour, secret)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodPost, "/rpc", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid JWT, got %d", rec.Code)
	}
}

// TestBearerAuth_JWTExpired verifies that a signed JWT with a past exp claim is rejected.
func TestBearerAuth_JWTExpired(t *testing.T) {
	secret := []byte("a-32-byte-hmac-secret-for-testing!!")
	now := time.Now()
	jwt := makeSignedJWT(t, secret, now.Add(-1*time.Minute).Unix(), now.Add(-2*time.Minute).Unix())
	mw := newBearerAuthMiddleware("", 24*time.Hour, secret)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodPost, "/rpc", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired JWT, got %d", rec.Code)
	}
}

// TestBearerAuth_JWTTooOld verifies that a signed JWT older than maxAge is rejected.
func TestBearerAuth_JWTTooOld(t *testing.T) {
	secret := []byte("a-32-byte-hmac-secret-for-testing!!")
	now := time.Now()
	// iat is 2 hours ago, exp is still in future, but maxAge is 1 hour.
	jwt := makeSignedJWT(t, secret, now.Add(1*time.Hour).Unix(), now.Add(-2*time.Hour).Unix())
	mw := newBearerAuthMiddleware("", 1*time.Hour, secret)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodPost, "/rpc", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for too-old JWT, got %d", rec.Code)
	}
}

// TestBearerAuth_JWTInvalidFormat verifies that a JWT-like token with invalid base64
// header encoding is rejected in JWT signature mode.
func TestBearerAuth_JWTInvalidFormat(t *testing.T) {
	secret := []byte("a-32-byte-hmac-secret-for-testing!!")
	token := "not!valid!base64.payload.signature"
	mw := newBearerAuthMiddleware("", 1*time.Hour, secret)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodPost, "/rpc", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid JWT format, got %d", rec.Code)
	}
}

// TestBearerAuth_PlainTokenNoExpiry verifies that plain (non-JWT) tokens work
// in plain bearer mode (no jwtSecret configured).
func TestBearerAuth_PlainTokenNoExpiry(t *testing.T) {
	token := "plaintextsecret"
	mw := newBearerAuthMiddleware(token, 1*time.Hour, nil)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodPost, "/rpc", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for plain token with expiry set, got %d", rec.Code)
	}
}

// TestBearerAuth_JWTSignatureValid verifies that a correctly signed HS256 JWT is accepted.
func TestBearerAuth_JWTSignatureValid(t *testing.T) {
	secret := []byte("32-bytes-of-hmac-secret-for-test!")
	now := time.Now()
	jwt := makeSignedJWT(t, secret, now.Add(1*time.Hour).Unix(), now.Add(-1*time.Minute).Unix())
	mw := newBearerAuthMiddleware("", 24*time.Hour, secret)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodPost, "/rpc", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid signed JWT, got %d", rec.Code)
	}
}

// TestBearerAuth_JWTSignatureInvalid verifies that a JWT signed with the wrong secret is rejected.
func TestBearerAuth_JWTSignatureInvalid(t *testing.T) {
	secret := []byte("32-bytes-of-hmac-secret-for-test!")
	wrongSecret := []byte("32-bytes-of-wrong-secret-for-test")
	now := time.Now()
	jwt := makeSignedJWT(t, wrongSecret, now.Add(1*time.Hour).Unix(), now.Add(-1*time.Minute).Unix())
	mw := newBearerAuthMiddleware("", 24*time.Hour, secret)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodPost, "/rpc", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for JWT with wrong secret, got %d", rec.Code)
	}
}

// TestBearerAuth_JWTForgedNoSignature verifies that a JWT with an empty signature is rejected.
func TestBearerAuth_JWTForgedNoSignature(t *testing.T) {
	secret := []byte("32-bytes-of-hmac-secret-for-test!")
	now := time.Now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"iat":%d}`, now.Add(1*time.Hour).Unix(), now.Unix())))
	// Empty signature segment — forged token.
	forgedJWT := header + "." + payload + "."
	mw := newBearerAuthMiddleware("", 24*time.Hour, secret)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodPost, "/rpc", nil)
	req.Header.Set("Authorization", "Bearer "+forgedJWT)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for forged JWT with empty signature, got %d", rec.Code)
	}
}

// TestBearerAuth_PlainTokenRejectsJWTLookalike verifies the fail-secure invariant:
// in plain bearer token mode, any token that looks like a JWT is rejected with 401.
func TestBearerAuth_PlainTokenRejectsJWTLookalike(t *testing.T) {
	secret := []byte("32-bytes-of-hmac-secret-for-test!")
	now := time.Now()
	// A properly signed JWT sent to a plain-token-mode server.
	jwt := makeSignedJWT(t, secret, now.Add(1*time.Hour).Unix(), now.Add(-1*time.Minute).Unix())
	mw := newBearerAuthMiddleware("someplaintoken", 0, nil)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodPost, "/rpc", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for JWT-lookalike in plain token mode, got %d", rec.Code)
	}
}

// TestBearerAuth_JWTWrongAlgorithm verifies that a JWT with alg:none is rejected.
func TestBearerAuth_JWTWrongAlgorithm(t *testing.T) {
	secret := []byte("32-bytes-of-hmac-secret-for-test!")
	now := time.Now()
	// makeUnsignedJWT uses alg:none with empty signature.
	algNoneJWT := makeUnsignedJWT(now.Add(1*time.Hour).Unix(), now.Add(-1*time.Minute).Unix())
	mw := newBearerAuthMiddleware("", 24*time.Hour, secret)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodPost, "/rpc", nil)
	req.Header.Set("Authorization", "Bearer "+algNoneJWT)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for JWT with alg:none, got %d", rec.Code)
	}
}

func TestCORS_MultipleOrigins_Allowed(t *testing.T) {
	t.Parallel()
	mw := newCORSMiddleware([]string{"https://app.example.com", "https://staging.example.com"})
	handler := mw(http.HandlerFunc(okHandler))

	for _, origin := range []string{"https://app.example.com", "https://staging.example.com"} {
		req := httptest.NewRequest(http.MethodGet, "/rpc", nil)
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Errorf("origin %s: expected ACAO=%s, got %q", origin, origin, got)
		}
	}
}

func TestCORS_MultipleOrigins_Rejected(t *testing.T) {
	t.Parallel()
	mw := newCORSMiddleware([]string{"https://app.example.com", "https://staging.example.com"})
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/rpc", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("unknown origin: expected no CORS headers, got %q", got)
	}
}

func TestCORS_SingleOrigin_BackwardCompat(t *testing.T) {
	t.Parallel()
	mw := newCORSMiddleware([]string{"https://example.com"})
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/rpc", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Errorf("expected ACAO=https://example.com, got %q", got)
	}
}
