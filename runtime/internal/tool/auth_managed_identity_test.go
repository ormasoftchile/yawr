package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ─── helpers ────────────────────────────────────────────────────────────────

// makeIMDSResponse returns a successful IMDS JSON response body.
func makeIMDSResponse(token string, expiresIn time.Duration) []byte {
	ts := strconv.FormatInt(time.Now().Add(expiresIn).Unix(), 10)
	b, _ := json.Marshal(map[string]string{
		"access_token": token,
		"expires_on":   ts,
		"token_type":   "Bearer",
	})
	return b
}

// makeIMDSHTTPClient wraps an httptest.Server so the provider calls it instead
// of the real IMDS endpoint. The provider builds the URL itself, so we
// substitute at the Do() layer.
func makeIMDSHTTPClient(srv *httptest.Server) imdsHTTPClient {
	return func(req *http.Request) (*http.Response, error) {
		// Rewrite the URL to point at the test server; keep query and headers.
		newURL := srv.URL + req.URL.RequestURI()
		newReq, err := http.NewRequestWithContext(req.Context(), req.Method, newURL, nil)
		if err != nil {
			return nil, err
		}
		for k, vs := range req.Header {
			for _, v := range vs {
				newReq.Header.Add(k, v)
			}
		}
		return srv.Client().Do(newReq)
	}
}

// makeErrorHTTPClient returns a client that always fails with the given error.
func makeErrorHTTPClient(err error) imdsHTTPClient {
	return func(_ *http.Request) (*http.Response, error) {
		return nil, err
	}
}

// bodyResponse returns a fixed-body HTTP response.
func bodyResponse(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
	}
}

// makeStaticHTTPClient returns a client that always responds with status/body.
func makeStaticHTTPClient(status int, body []byte) imdsHTTPClient {
	return func(_ *http.Request) (*http.Response, error) {
		return bodyResponse(status, body), nil
	}
}

// ─── happy path ─────────────────────────────────────────────────────────────

func TestManagedIdentity_HappyPath(t *testing.T) {
	const wantToken = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.test-system-assigned"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata") != "true" {
			t.Errorf("expected Metadata: true header, got %q", r.Header.Get("Metadata"))
		}
		if v := r.URL.Query().Get("api-version"); v != imdsAPIVersion {
			t.Errorf("expected api-version=%s, got %s", imdsAPIVersion, v)
		}
		if v := r.URL.Query().Get("resource"); v == "" {
			t.Errorf("resource query param is missing")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(makeIMDSResponse(wantToken, 70*time.Minute))
	}))
	defer srv.Close()

	p := NewManagedIdentityAuthProvider("api://test/scope", "")
	p.httpClient = makeIMDSHTTPClient(srv)

	tok, err := p.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != wantToken {
		t.Errorf("got token %q, want %q", tok, wantToken)
	}
}

// ─── user-assigned identity (client_id param) ────────────────────────────────

func TestManagedIdentity_UserAssignedClientID(t *testing.T) {
	const wantClientID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const wantToken = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.test-user-assigned"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("client_id"); got != wantClientID {
			t.Errorf("expected client_id=%s, got %s", wantClientID, got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(makeIMDSResponse(wantToken, 70*time.Minute))
	}))
	defer srv.Close()

	p := NewManagedIdentityAuthProvider("api://test/scope", wantClientID)
	p.httpClient = makeIMDSHTTPClient(srv)

	tok, err := p.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != wantToken {
		t.Errorf("got token %q, want %q", tok, wantToken)
	}
}

// ─── non-200 response ───────────────────────────────────────────────────────

func TestManagedIdentity_Non200Response(t *testing.T) {
	p := NewManagedIdentityAuthProvider("api://test/scope", "")
	p.httpClient = makeStaticHTTPClient(http.StatusBadRequest,
		[]byte(`{"error":"invalid_request","error_description":"missing resource"}`))

	_, err := p.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
	if !strings.Contains(err.Error(), "MCP-007") {
		t.Errorf("expected MCP-007, got: %v", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("expected status code in error, got: %v", err)
	}
	// The raw response body must NOT appear in the error (B-24: could contain
	// partial token or security-sensitive diagnostics).
	if strings.Contains(err.Error(), "invalid_request") {
		t.Errorf("raw response body leaked into error: %v", err)
	}
}

func TestManagedIdentity_Non200DoesNotLeakBody(t *testing.T) {
	// A 500 response might contain a partial token in its body (e.g., a
	// logging middleware that echoes headers). The error must never include it.
	const sensitiveBody = `{"access_token":"SECRET-LEAK","error":"internal"}`
	p := NewManagedIdentityAuthProvider("api://test/scope", "")
	p.httpClient = makeStaticHTTPClient(http.StatusInternalServerError, []byte(sensitiveBody))

	_, err := p.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if strings.Contains(err.Error(), "SECRET-LEAK") {
		t.Errorf("token from error response body leaked into error message: %v", err)
	}
}

// ─── malformed JSON ─────────────────────────────────────────────────────────

func TestManagedIdentity_MalformedJSON(t *testing.T) {
	p := NewManagedIdentityAuthProvider("api://test/scope", "")
	p.httpClient = makeStaticHTTPClient(http.StatusOK, []byte(`not-json{`))

	_, err := p.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "MCP-007") {
		t.Errorf("expected MCP-007, got: %v", err)
	}
}

// ─── context cancellation mid-request ────────────────────────────────────────

func TestManagedIdentity_ContextCancellation(t *testing.T) {
	// A server that blocks until the test tells it to proceed.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	p := NewManagedIdentityAuthProvider("api://test/scope", "")
	p.httpClient = makeIMDSHTTPClient(srv)

	errc := make(chan error, 1)
	go func() {
		_, err := p.Token(ctx)
		errc <- err
	}()

	// Cancel after a brief moment while the goroutine is blocked in the handler.
	time.Sleep(20 * time.Millisecond)
	cancel()

	err := <-errc
	if err == nil {
		t.Fatal("expected error after context cancellation")
	}
	if !strings.Contains(err.Error(), "MCP-007") {
		t.Errorf("expected MCP-007 on cancellation, got: %v", err)
	}
}

// ─── token cache hit ────────────────────────────────────────────────────────

func TestManagedIdentity_CacheHit(t *testing.T) {
	callCount := 0
	const wantToken = "cached-token-value"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.Write(makeIMDSResponse(wantToken, 70*time.Minute))
	}))
	defer srv.Close()

	p := NewManagedIdentityAuthProvider("api://test/scope", "")
	p.httpClient = makeIMDSHTTPClient(srv)

	tok1, err := p.Token(context.Background())
	if err != nil {
		t.Fatalf("first Token() call failed: %v", err)
	}
	tok2, err := p.Token(context.Background())
	if err != nil {
		t.Fatalf("second Token() call failed: %v", err)
	}

	if tok1 != wantToken || tok2 != wantToken {
		t.Errorf("unexpected tokens: tok1=%q tok2=%q", tok1, tok2)
	}
	if callCount != 1 {
		t.Errorf("expected exactly 1 IMDS call, got %d", callCount)
	}
}

// ─── expiry-triggered refresh ────────────────────────────────────────────────

func TestManagedIdentity_ExpiryTriggeredRefresh(t *testing.T) {
	callCount := 0
	tokens := []string{"first-token", "second-token"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		tok := tokens[0]
		if callCount < len(tokens) {
			tok = tokens[callCount]
		}
		callCount++
		// Expiry well past the refresh buffer — second call happens only
		// after we manually force expiry below.
		w.Write(makeIMDSResponse(tok, 70*time.Minute))
	}))
	defer srv.Close()

	p := NewManagedIdentityAuthProvider("api://test/scope", "")
	p.httpClient = makeIMDSHTTPClient(srv)

	tok1, err := p.Token(context.Background())
	if err != nil {
		t.Fatalf("first Token() failed: %v", err)
	}
	if tok1 != "first-token" {
		t.Errorf("want first-token, got %q", tok1)
	}

	// Force expiry so next call must re-acquire.
	p.mu.Lock()
	p.expiry = time.Now().Add(-1 * time.Minute)
	p.mu.Unlock()

	tok2, err := p.Token(context.Background())
	if err != nil {
		t.Fatalf("second Token() failed: %v", err)
	}
	if tok2 != "second-token" {
		t.Errorf("want second-token after expiry, got %q", tok2)
	}
	if callCount != 2 {
		t.Errorf("expected 2 IMDS calls total, got %d", callCount)
	}
}

// ─── IMDS unreachable (no network error) ────────────────────────────────────

func TestManagedIdentity_IMDSUnreachable(t *testing.T) {
	p := NewManagedIdentityAuthProvider("api://test/scope", "")
	p.httpClient = makeErrorHTTPClient(fmt.Errorf("dial tcp 169.254.169.254:80: i/o timeout"))

	_, err := p.Token(context.Background())
	if err == nil {
		t.Fatal("expected error when IMDS unreachable")
	}
	if !strings.Contains(err.Error(), "MCP-007") {
		t.Errorf("expected MCP-007, got: %v", err)
	}
}

// ─── token never logged ─────────────────────────────────────────────────────

func TestManagedIdentity_TokenNotInError(t *testing.T) {
	const secretToken = "SUPERSECRET-TOKEN-VALUE"

	// Return a valid token, then force expiry and fail the next request.
	p := NewManagedIdentityAuthProvider("api://test/scope", "")
	callCount := 0
	p.httpClient = func(req *http.Request) (*http.Response, error) {
		callCount++
		if callCount == 1 {
			return bodyResponse(http.StatusOK, makeIMDSResponse(secretToken, 70*time.Minute)), nil
		}
		// On refresh attempt, return a non-200 with body that echoes the old token.
		body := fmt.Sprintf(`{"error":"token_expired","last_token":"%s"}`, secretToken)
		return bodyResponse(http.StatusUnauthorized, []byte(body)), nil
	}

	// Acquire the initial token.
	tok, err := p.Token(context.Background())
	if err != nil || tok != secretToken {
		t.Fatalf("initial acquisition failed or wrong token: tok=%q err=%v", tok, err)
	}

	// Force expiry so the next call must hit IMDS again.
	p.mu.Lock()
	p.expiry = time.Now().Add(-1 * time.Minute)
	p.mu.Unlock()

	_, err = p.Token(context.Background())
	if err == nil {
		t.Fatal("expected error on failed refresh")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Errorf("token leaked into error message:\n  error: %v", err)
	}
}

// ─── parseIMDSExpiry ─────────────────────────────────────────────────────────

func TestParseIMDSExpiry_UnixTimestamp(t *testing.T) {
	future := time.Now().Add(70 * time.Minute)
	ts := strconv.FormatInt(future.Unix(), 10)
	got := parseIMDSExpiry(ts, "")
	diff := got.Sub(future)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("expiry delta too large: %v", diff)
	}
}

func TestParseIMDSExpiry_ExpiresIn(t *testing.T) {
	got := parseIMDSExpiry("", "3600")
	// Should be roughly now + 3600s.
	expected := time.Now().Add(3600 * time.Second)
	diff := got.Sub(expected)
	if diff < -2*time.Second || diff > 2*time.Second {
		t.Errorf("expires_in parse off by %v", diff)
	}
}

func TestParseIMDSExpiry_FallbackOnBadInput(t *testing.T) {
	got := parseIMDSExpiry("bad", "also-bad")
	if got.Before(time.Now().Add(49*time.Minute)) || got.After(time.Now().Add(51*time.Minute)) {
		t.Errorf("expected fallback to ~50 minutes, got expiry in %v", time.Until(got))
	}
}
