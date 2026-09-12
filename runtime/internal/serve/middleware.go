package serve

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type contextKey string

const requestIDKey contextKey = "requestID"

type middleware func(http.Handler) http.Handler

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	stack := []middleware{
		requestIDMiddleware,
		loggingMiddleware,
		newCORSMiddleware(s.cfg.AllowedOrigins),
		newRateLimitMiddleware(s.cfg.RateLimit, s.cfg.TrustProxyHeaders),
		newBearerAuthMiddleware(s.cfg.BearerToken, s.cfg.BearerTokenExpiry, s.cfg.JWTSecret),
		recoveryMiddleware,
	}
	handler := next
	for i := len(stack) - 1; i >= 0; i-- {
		handler = stack[i](handler)
	}
	return handler
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uuid.New().String()
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}

func (r *statusRecorder) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := r.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return http.ErrNotSupported
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func loggingMiddleware(next http.Handler) http.Handler {
	logger := log.Default()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		duration := time.Since(start).Milliseconds()
		entry := map[string]any{
			"method":      r.Method,
			"path":        r.URL.Path,
			"status":      rec.status,
			"duration_ms": duration,
		}
		if reqID, ok := r.Context().Value(requestIDKey).(string); ok {
			entry["request_id"] = reqID
		}
		payload, _ := json.Marshal(entry)
		logger.Printf("%s", payload)
	})
}

// newCORSMiddleware returns a middleware that adds CORS headers.
// When allowedOrigins is empty, all origins are permitted.
// When non-empty, only requests from listed origins receive CORS headers.
func newCORSMiddleware(allowedOrigins []string) middleware {
	originSet := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		originSet[o] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && (len(originSet) == 0 || originSet[origin]) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Last-Event-ID, X-Request-ID")
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// newBearerAuthMiddleware returns a middleware that validates Authorization: Bearer <token>.
// The /health path is always exempt from authentication.
//
// Three modes based on configuration:
//   - token="" && jwtSecret==nil: open access (no auth)
//   - token!="" && jwtSecret==nil: plain bearer token mode; JWT-lookalike tokens are
//     rejected with 401 (fail-secure) to prevent accidental unverified JWT acceptance.
//   - token=="" && jwtSecret!=nil: JWT mode; HMAC-SHA256 signature is verified and
//     exp/iat claims are validated against expiry.
func newBearerAuthMiddleware(token string, expiry time.Duration, jwtSecret []byte) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if (token == "" && jwtSecret == nil) || r.URL.Path == "/health" {
				next.ServeHTTP(w, r)
				return
			}
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			provided := strings.TrimPrefix(auth, "Bearer ")

			if jwtSecret != nil {
				// JWT signature verification mode.
				if err := verifyJWT(provided, jwtSecret, expiry); err != nil {
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			// Plain bearer token mode.
			// Fail-secure: reject JWT-lookalikes to prevent unverified JWT acceptance.
			if looksLikeJWT(provided) {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			// Constant-time comparison prevents timing attacks.
			if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// looksLikeJWT reports whether s has three non-empty dot-separated segments,
// as expected for a properly formed JWT.
func looksLikeJWT(s string) bool {
	parts := strings.SplitN(s, ".", 4)
	return len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != ""
}

// verifyJWT verifies the HMAC-SHA256 signature of a JWT and validates its time claims.
// Only HS256 algorithm is accepted. maxAge of 0 disables the iat check.
func verifyJWT(token string, secret []byte, maxAge time.Duration) error {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return errors.New("invalid JWT format")
	}
	header, payload, signature := parts[0], parts[1], parts[2]

	headerJSON, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		return fmt.Errorf("invalid header encoding: %w", err)
	}
	var hdr struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(headerJSON, &hdr); err != nil {
		return fmt.Errorf("invalid header: %w", err)
	}
	if hdr.Alg != "HS256" {
		return fmt.Errorf("unsupported algorithm: %s", hdr.Alg)
	}

	signingInput := header + "." + payload
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	expectedSig := mac.Sum(nil)

	providedSig, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("invalid signature encoding: %w", err)
	}

	if !hmac.Equal(expectedSig, providedSig) {
		return errors.New("invalid signature")
	}

	return validateJWTExpiry(token, maxAge)
}

// jwtTimeClaims holds the standard time claims extracted from a JWT payload.
type jwtTimeClaims struct {
	EXP int64 `json:"exp"`
	IAT int64 `json:"iat"`
}

// validateJWTExpiry decodes the middle segment of a JWT and checks expiry.
// It returns an error if the exp claim is in the past, or if the iat claim
// indicates the token is older than maxAge.
func validateJWTExpiry(token string, maxAge time.Duration) error {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return errors.New("invalid JWT format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("invalid JWT payload encoding: %w", err)
	}
	var claims jwtTimeClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return fmt.Errorf("invalid JWT payload: %w", err)
	}
	now := time.Now().Unix()
	if claims.EXP != 0 && claims.EXP < now {
		return errors.New("token expired")
	}
	if claims.IAT != 0 && maxAge > 0 {
		if time.Since(time.Unix(claims.IAT, 0)) > maxAge {
			return errors.New("token too old")
		}
	}
	return nil
}

func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
