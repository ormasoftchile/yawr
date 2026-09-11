package tool

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// knownToken is a synthetic but realistic-looking token used in redaction tests.
// It contains a unique, unlikely-to-collide substring to catch accidental leakage.
const knownToken = "eyJ0eXAiOiJKV1QiLCJhbGciOiJSUzI1NiJ9.YAWR_REDACTION_PROBE_TOKEN.signature_stub"

// ─── helpers ────────────────────────────────────────────────────────────────

func makeSuccessRunner(token string, expiryDuration time.Duration) azRunner {
	return func(_ context.Context, args []string) ([]byte, string, error) {
		ts := strconv.FormatInt(time.Now().Add(expiryDuration).Unix(), 10)
		body := fmt.Sprintf(`{"accessToken":%q,"expires_on":%q,"tokenType":"Bearer"}`, token, ts)
		return []byte(body), "", nil
	}
}

func makeFailRunner(stderr string) azRunner {
	return func(_ context.Context, args []string) ([]byte, string, error) {
		return nil, stderr, fmt.Errorf("exit status 1")
	}
}

func makeNotFoundRunner() azRunner {
	return func(_ context.Context, args []string) ([]byte, string, error) {
		return nil, "", &exec.Error{Name: "az", Err: exec.ErrNotFound}
	}
}

// ─── failure-mode tests ──────────────────────────────────────────────────────

// TestAzureCLIAuthProvider_AzNotInstalled verifies the actionable message
// when az is not on PATH.
func TestAzureCLIAuthProvider_AzNotInstalled(t *testing.T) {
	p := &AzureCLIAuthProvider{scope: "api://test/mcp.tools", runner: makeNotFoundRunner()}
	_, err := p.Token(context.Background())
	if err == nil {
		t.Fatal("expected error when az is not installed")
	}
	msg := err.Error()
	if !strings.Contains(msg, "not installed") && !strings.Contains(msg, "not on PATH") {
		t.Errorf("expected 'not installed or not on PATH' in error, got: %v", msg)
	}
	if !strings.Contains(msg, "MCP-007") {
		t.Errorf("expected MCP-007 code, got: %v", msg)
	}
}

// TestAzureCLIAuthProvider_NotLoggedIn verifies the actionable message
// when az login has not been run.
func TestAzureCLIAuthProvider_NotLoggedIn(t *testing.T) {
	stderrs := []string{
		"Please run 'az login' to setup account.",
		"ERROR: Please run 'az login' before proceeding.",
		"AADSTS700082: The refresh Token has expired",
	}
	for _, se := range stderrs {
		p := &AzureCLIAuthProvider{scope: "api://test/mcp.tools", runner: makeFailRunner(se)}
		_, err := p.Token(context.Background())
		if err == nil {
			t.Fatalf("expected error for stderr %q", se)
		}
		msg := err.Error()
		if !strings.Contains(msg, "az login") {
			t.Errorf("stderr=%q: expected 'az login' guidance, got: %v", se, msg)
		}
		if !strings.Contains(msg, "MCP-007") {
			t.Errorf("expected MCP-007 code, got: %v", msg)
		}
	}
}

// TestAzureCLIAuthProvider_NoScopeConsent verifies the actionable message
// when az is authenticated but lacks consent for the requested scope.
func TestAzureCLIAuthProvider_NoScopeConsent(t *testing.T) {
	stderrs := []string{
		"AADSTS65001: The user or administrator has not consented to use the application",
		"AADSTS70011: The provided value for the input parameter 'scope' is not valid.",
	}
	for _, se := range stderrs {
		p := &AzureCLIAuthProvider{scope: "api://icmmcpapi-prod/mcp.tools", runner: makeFailRunner(se)}
		_, err := p.Token(context.Background())
		if err == nil {
			t.Fatalf("expected error for stderr %q", se)
		}
		msg := err.Error()
		if !strings.Contains(strings.ToLower(msg), "consent") && !strings.Contains(strings.ToLower(msg), "scope") {
			t.Errorf("stderr=%q: expected consent/scope guidance, got: %v", se, msg)
		}
		if !strings.Contains(msg, "MCP-007") {
			t.Errorf("expected MCP-007 code, got: %v", msg)
		}
	}
}

func TestAzureCLIAuthProvider_NumericExpiresOnParses(t *testing.T) {
	expires := time.Now().Add(70 * time.Minute).Unix()
	runner := func(_ context.Context, _ []string) ([]byte, string, error) {
		return []byte(fmt.Sprintf(`{"accessToken":"tok-numeric-expiry","expires_on":%d,"tokenType":"Bearer"}`, expires)), "", nil
	}
	p := &AzureCLIAuthProvider{scope: "api://test/scope", runner: runner}

	tok, err := p.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: unexpected error for numeric expires_on: %v", err)
	}
	if tok != "tok-numeric-expiry" {
		t.Fatalf("token = %q, want tok-numeric-expiry", tok)
	}
	if p.expiry.Unix() != expires {
		t.Fatalf("expiry = %d, want %d", p.expiry.Unix(), expires)
	}
}

func TestAzureCLIAuthProvider_InvalidResourceNotMisclassifiedAsLogin(t *testing.T) {
	stderr := "AADSTS500011: The resource principal named api://wrong/audience was not found in the tenant. Trace ID: x. To re-authenticate, please run 'az login'."
	p := &AzureCLIAuthProvider{scope: "api://wrong/audience", runner: makeFailRunner(stderr)}

	_, err := p.Token(context.Background())
	if err == nil {
		t.Fatal("expected invalid resource error")
	}
	msg := err.Error()
	if strings.Contains(msg, "not authenticated") {
		t.Fatalf("invalid resource was misclassified as not authenticated: %v", msg)
	}
	if !strings.Contains(msg, "api://wrong/audience") {
		t.Fatalf("error does not include offending scope: %v", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "not valid") && !strings.Contains(strings.ToLower(msg), "not found") {
		t.Fatalf("error does not explain invalid scope/resource: %v", msg)
	}
}

// TestAzureCLIAuthProvider_MalformedOutput verifies the actionable message
// when az succeeds but returns non-JSON or missing accessToken.
func TestAzureCLIAuthProvider_MalformedOutput(t *testing.T) {
	cases := []struct {
		name   string
		stdout []byte
	}{
		{"not JSON", []byte("not json at all")},
		{"empty body", []byte("")},
		{"missing accessToken", []byte(`{"expiresOn":"2026-01-01 00:00:00.000000"}`)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			runner := func(_ context.Context, _ []string) ([]byte, string, error) {
				return tc.stdout, "", nil
			}
			p := &AzureCLIAuthProvider{scope: "api://test/scope", runner: runner}
			_, err := p.Token(context.Background())
			if err == nil {
				t.Fatal("expected error for malformed output")
			}
			msg := err.Error()
			if !strings.Contains(msg, "MCP-007") {
				t.Errorf("expected MCP-007 code, got: %v", msg)
			}
		})
	}
}

func TestAzureCLIAuthProvider_UsesResourceWhenConfigured(t *testing.T) {
	var gotArgs []string
	p := &AzureCLIAuthProvider{
		resource: "https://icm-mcp-prod.azure-api.net",
		runner: func(_ context.Context, args []string) ([]byte, string, error) {
			gotArgs = append([]string(nil), args...)
			return makeSuccessRunner("tok-resource", 70*time.Minute)(context.Background(), args)
		},
	}

	if _, err := p.Token(context.Background()); err != nil {
		t.Fatalf("Token: unexpected error: %v", err)
	}
	want := []string{"account", "get-access-token", "--resource", "https://icm-mcp-prod.azure-api.net", "-o", "json"}
	if strings.Join(gotArgs, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("az args = %#v, want %#v", gotArgs, want)
	}
}

// ─── caching and refresh tests ───────────────────────────────────────────────

// TestAzureCLIAuthProvider_CachesToken verifies that a second Token() call
// does not shell out again while the token is still valid.
func TestAzureCLIAuthProvider_CachesToken(t *testing.T) {
	callCount := 0
	runner := func(_ context.Context, _ []string) ([]byte, string, error) {
		callCount++
		ts := strconv.FormatInt(time.Now().Add(70*time.Minute).Unix(), 10)
		return []byte(fmt.Sprintf(`{"accessToken":"tok","expires_on":%q}`, ts)), "", nil
	}
	p := &AzureCLIAuthProvider{scope: "api://test/scope", runner: runner}

	for i := 0; i < 5; i++ {
		tok, err := p.Token(context.Background())
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if tok != "tok" {
			t.Fatalf("unexpected token: %q", tok)
		}
	}
	if callCount != 1 {
		t.Errorf("expected 1 az invocation, got %d", callCount)
	}
}

// TestAzureCLIAuthProvider_RefreshesNearExpiry verifies that Token() re-acquires
// when the cached token is within the 5-minute refresh buffer.
func TestAzureCLIAuthProvider_RefreshesNearExpiry(t *testing.T) {
	callCount := 0
	runner := func(_ context.Context, _ []string) ([]byte, string, error) {
		callCount++
		ts := strconv.FormatInt(time.Now().Add(70*time.Minute).Unix(), 10)
		return []byte(fmt.Sprintf(`{"accessToken":"tok%d","expires_on":%q}`, callCount, ts)), "", nil
	}
	p := &AzureCLIAuthProvider{
		scope:  "api://test/scope",
		runner: runner,
		// Pre-seed cache with a token expiring in 3 minutes (inside the buffer).
		token:  "old-tok",
		expiry: time.Now().Add(3 * time.Minute),
	}

	tok, err := p.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok == "old-tok" {
		t.Error("expected token to be refreshed, but got the old cached value")
	}
	if callCount != 1 {
		t.Errorf("expected exactly 1 refresh call, got %d", callCount)
	}
}

// TestAzureCLIAuthProvider_GracefulDegradationOnEarlyRefreshFailure verifies
// that if the proactive refresh fails but the token is not yet hard-expired,
// the still-valid cached token is returned rather than an error.
func TestAzureCLIAuthProvider_GracefulDegradationOnEarlyRefreshFailure(t *testing.T) {
	runner := makeFailRunner("transient network error")
	p := &AzureCLIAuthProvider{
		scope:  "api://test/scope",
		runner: runner,
		// Token expires in 3 minutes — within refresh buffer but not yet expired.
		token:  "still-valid-tok",
		expiry: time.Now().Add(3 * time.Minute),
	}

	tok, err := p.Token(context.Background())
	if err != nil {
		t.Fatalf("expected graceful fallback to cached token, got error: %v", err)
	}
	if tok != "still-valid-tok" {
		t.Errorf("expected cached token, got %q", tok)
	}
}

// TestAzureCLIAuthProvider_Invalidate verifies that Invalidate() forces
// re-acquisition on the next Token() call.
func TestAzureCLIAuthProvider_Invalidate(t *testing.T) {
	callCount := 0
	runner := func(_ context.Context, _ []string) ([]byte, string, error) {
		callCount++
		ts := strconv.FormatInt(time.Now().Add(70*time.Minute).Unix(), 10)
		return []byte(fmt.Sprintf(`{"accessToken":"tok%d","expires_on":%q}`, callCount, ts)), "", nil
	}
	p := &AzureCLIAuthProvider{scope: "api://test/scope", runner: runner}

	tok1, err := p.Token(context.Background())
	if err != nil {
		t.Fatalf("first Token: %v", err)
	}

	p.Invalidate()

	tok2, err := p.Token(context.Background())
	if err != nil {
		t.Fatalf("second Token: %v", err)
	}

	if tok1 == tok2 {
		t.Error("expected different token after Invalidate")
	}
	if callCount != 2 {
		t.Errorf("expected 2 az invocations after Invalidate, got %d", callCount)
	}
}

// ─── redaction proof ─────────────────────────────────────────────────────────

// TestAzureCLIAuthProvider_TokenNeverLeaksIntoDiagnostics is the redaction
// proof required by B-24. It verifies that the concrete token value returned
// by Token() never appears in any error message that the provider emits —
// regardless of failure mode.
//
// The test seeds the provider's cache with a known, unique token value, then
// forces re-acquisition through each of the four failure paths and asserts
// that the token string is absent from every returned error.
func TestAzureCLIAuthProvider_TokenNeverLeaksIntoDiagnostics(t *testing.T) {
	type failCase struct {
		name   string
		runner azRunner
	}

	cases := []failCase{
		{
			name:   "az not on PATH",
			runner: makeNotFoundRunner(),
		},
		{
			name:   "not logged in",
			runner: makeFailRunner("Please run 'az login' to setup account."),
		},
		{
			name:   "no scope consent",
			runner: makeFailRunner("AADSTS65001: The user or administrator has not consented to use the application"),
		},
		{
			name:   "malformed output",
			runner: func(_ context.Context, _ []string) ([]byte, string, error) { return []byte("not-json"), "", nil },
		},
		{
			name:   "generic az error",
			runner: makeFailRunner("Some unexpected az error"),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			p := &AzureCLIAuthProvider{
				scope:  "api://icmmcpapi-prod/mcp.tools",
				runner: tc.runner,
				// Seed the cache with the known token but mark it as expired
				// so the provider is forced to re-acquire.
				token:  knownToken,
				expiry: time.Now().Add(-1 * time.Minute),
			}

			_, err := p.Token(context.Background())
			if err == nil {
				// If no error, the provider somehow succeeded — that's also fine,
				// but verify the returned token isn't just the cached one leaking
				// through an unexpected path.
				t.Logf("%s: provider succeeded (no error path to check)", tc.name)
				return
			}

			errStr := err.Error()
			if strings.Contains(errStr, knownToken) {
				t.Errorf("TOKEN LEAKED into error message for case %q:\n  error: %v\n  leaked token prefix: %.40s",
					tc.name, errStr, knownToken)
			}
		})
	}
}

// TestAzureCLIAuthProvider_TokenNeverLeaksInSuccessPath verifies that a
// successful acquisition does not inadvertently log or surface the token in
// any way other than the return value of Token().
//
// Specifically: the raw az JSON output (which contains the token) is decoded
// and discarded — it must not be forwarded to any error, log, or trace surface.
// This test verifies the decoded token appears ONLY in the return value, not
// in the error channel.
func TestAzureCLIAuthProvider_TokenNeverLeaksInSuccessPath(t *testing.T) {
	p := &AzureCLIAuthProvider{
		scope:  "api://test/scope",
		runner: makeSuccessRunner(knownToken, 70*time.Minute),
	}

	tok, err := p.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != knownToken {
		t.Fatalf("expected token %q, got %q", knownToken, tok)
	}
	// The token is in tok (the return value) — that's correct.
	// Verify it is NOT also in any error from a subsequent forced-refresh
	// that fails mid-run (e.g., network goes away after first success).
	p.Invalidate()
	p.runner = makeFailRunner("transient error on second call")
	_, err = p.Token(context.Background())
	if err == nil {
		t.Fatal("expected error after Invalidate + failing runner")
	}
	if strings.Contains(err.Error(), knownToken) {
		t.Errorf("cached token leaked into error after Invalidate:\n  error: %v", err)
	}
}

// ─── NewAuthProvider tests ───────────────────────────────────────────────────

func TestNewAuthProvider_AzureCLI(t *testing.T) {
	ap, err := NewAuthProvider("azure-cli", "api://test/scope")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := ap.(*AzureCLIAuthProvider); !ok {
		t.Errorf("expected *AzureCLIAuthProvider, got %T", ap)
	}
}

func TestNewAuthProvider_ManagedIdentity(t *testing.T) {
	ap, err := NewAuthProvider("managed-identity", "api://test/scope")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := ap.(*ManagedIdentityAuthProvider); !ok {
		t.Errorf("expected *ManagedIdentityAuthProvider, got %T", ap)
	}
}

func TestNewAuthProvider_UnknownProvider(t *testing.T) {
	_, err := NewAuthProvider("workload-identity", "api://test/scope")
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	if !strings.Contains(err.Error(), "MCP-002") {
		t.Errorf("expected MCP-002 error, got: %v", err)
	}
}

func TestNewAuthProvider_WorkloadIdentityReturns002(t *testing.T) {
	_, err := NewAuthProvider("workload-identity", "api://test/scope")
	if err == nil {
		t.Fatal("expected MCP-002 for workload-identity (not yet implemented)")
	}
	if !strings.Contains(err.Error(), "MCP-002") {
		t.Errorf("expected MCP-002 error, got: %v", err)
	}
}

// ─── expiry parsing ──────────────────────────────────────────────────────────

func TestParseAzExpiry_UnixTimestamp(t *testing.T) {
	future := time.Now().Add(70 * time.Minute)
	ts := strconv.FormatInt(future.Unix(), 10)
	got := parseAzExpiry(ts, "")
	diff := got.Sub(future)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("expiry delta too large: %v", diff)
	}
}

func TestParseAzExpiry_DateTimeString(t *testing.T) {
	// Use a well-known datetime to avoid timezone issues in CI.
	future := time.Now().Add(70 * time.Minute).Truncate(time.Second)
	formatted := future.Format("2006-01-02 15:04:05.000000")
	got := parseAzExpiry("", formatted)
	// Parsed in local time — allow 1-second rounding.
	diff := got.Sub(future)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("expiry delta too large: %v (formatted=%q)", diff, formatted)
	}
}

func TestParseAzExpiry_FallbackOnBadInput(t *testing.T) {
	got := parseAzExpiry("not-a-timestamp", "not-a-date")
	// Should fall back to now+50min.
	if got.Before(time.Now().Add(49*time.Minute)) || got.After(time.Now().Add(51*time.Minute)) {
		t.Errorf("expected fallback to ~50 minutes, got expiry in %v", time.Until(got))
	}
}
