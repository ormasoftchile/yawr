package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
)

const tokenRefreshBuffer = 5 * time.Minute

// azTokenResponse is the shape returned by:
//
//	az account get-access-token --scope <scope> -o json
//	az account get-access-token --resource <resource> -o json
type azTokenResponse struct {
	AccessToken string `json:"accessToken"`
	ExpiresOnTS string `json:"expires_on"` // Unix timestamp string/number — preferred, TZ-safe
	ExpiresOn   string `json:"expiresOn"`  // "2006-01-02 15:04:05.000000" fallback
}

func (r *azTokenResponse) UnmarshalJSON(data []byte) error {
	var raw struct {
		AccessToken string          `json:"accessToken"`
		ExpiresOnTS json.RawMessage `json:"expires_on"`
		ExpiresOn   string          `json:"expiresOn"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.AccessToken = raw.AccessToken
	r.ExpiresOn = raw.ExpiresOn
	if len(raw.ExpiresOnTS) == 0 || string(raw.ExpiresOnTS) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw.ExpiresOnTS, &s); err == nil {
		r.ExpiresOnTS = s
		return nil
	}
	var n json.Number
	dec := json.NewDecoder(strings.NewReader(string(raw.ExpiresOnTS)))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return fmt.Errorf("expires_on must be string or number: %w", err)
	}
	r.ExpiresOnTS = n.String()
	return nil
}

// azRunner is the function type for executing az CLI commands.
// Replaced in tests with a stub; nil means use the real exec.Command.
type azRunner func(ctx context.Context, args []string) (stdout []byte, stderr string, err error)

// AzureCLIAuthProvider acquires bearer tokens via the Azure CLI.
//
// Caching strategy:
//   - Token is cached with its expiry parsed from az's output.
//   - Token() returns the cached value while now+5min < expiry.
//   - Token() proactively re-acquires when within 5 min of expiry.
//     If that early re-acquisition fails and the token is still valid
//     (not yet expired), the still-valid cached token is returned
//     rather than failing the call — this avoids dropping a live run
//     because of a transient az CLI hiccup.
//   - Once the token has expired, Token() MUST re-acquire and returns
//     an error on failure.
//   - Invalidate() clears the cache; the next Token() call re-acquires.
//     The HTTP transport calls this on receipt of HTTP 401.
type AzureCLIAuthProvider struct {
	scope    string
	resource string
	runner   azRunner // nil → realAzRunner

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// NewAzureCLIAuthProvider constructs an AzureCLIAuthProvider for the given scope.
func NewAzureCLIAuthProvider(scope string) *AzureCLIAuthProvider {
	return &AzureCLIAuthProvider{scope: scope}
}

// NewAzureCLIAuthProviderForResource constructs an AzureCLIAuthProvider for an
// Azure AD resource/audience. It passes the value through to az as --resource.
func NewAzureCLIAuthProviderForResource(resource string) *AzureCLIAuthProvider {
	return &AzureCLIAuthProvider{resource: resource}
}

// Token returns a valid bearer token for the configured scope or resource.
// It shells out to `az account get-access-token` at most once per token
// lifetime (minus the 5-minute refresh buffer).
func (p *AzureCLIAuthProvider) Token(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()

	// Still well inside the validity window — return cached.
	if p.token != "" && now.Add(tokenRefreshBuffer).Before(p.expiry) {
		return p.token, nil
	}

	// Proactive refresh: token exists but within the 5-min buffer or expired.
	newTok, err := p.acquire(ctx)
	if err != nil {
		// If the token hasn't hard-expired yet, return the still-valid cached
		// value and swallow the early-refresh error gracefully.
		if p.token != "" && now.Before(p.expiry) {
			return p.token, nil
		}
		return "", err
	}
	return newTok, nil
}

// Invalidate clears the cached token. The next Token() call will re-acquire.
// Call after receiving HTTP 401 to handle mid-run token rejection.
func (p *AzureCLIAuthProvider) Invalidate() {
	p.mu.Lock()
	p.token = ""
	p.expiry = time.Time{}
	p.mu.Unlock()
}

// acquire runs az and updates the cache. Caller must hold p.mu.
func (p *AzureCLIAuthProvider) acquire(ctx context.Context) (string, error) {
	targetFlag := "--scope"
	targetValue := p.scope
	if p.resource != "" {
		targetFlag = "--resource"
		targetValue = p.resource
	}
	args := []string{"account", "get-access-token", targetFlag, targetValue, "-o", "json"}

	run := p.runner
	if run == nil {
		run = realAzRunner
	}

	stdout, stderr, err := run(ctx, args)
	if err != nil {
		return "", p.classifyAzError(err, stderr)
	}

	var resp azTokenResponse
	if jsonErr := json.Unmarshal(stdout, &resp); jsonErr != nil {
		return "", errkit.New("MCP-007",
			fmt.Sprintf("mcp-http: failed to acquire auth token: az CLI returned malformed output: %v", jsonErr))
	}
	if resp.AccessToken == "" {
		return "", errkit.New("MCP-007",
			"mcp-http: failed to acquire auth token: az CLI output is missing accessToken field")
	}

	expiry := parseAzExpiry(resp.ExpiresOnTS, resp.ExpiresOn)
	// Store in cache — token value lives only in-memory.
	p.token = resp.AccessToken
	p.expiry = expiry
	return resp.AccessToken, nil
}

// classifyAzError maps the az command failure to an actionable MCP-007 error.
// The token value is never included — on failure, no token exists.
func (p *AzureCLIAuthProvider) authTarget() (string, string) {
	if p.resource != "" {
		return "resource", p.resource
	}
	return "scope", p.scope
}

func (p *AzureCLIAuthProvider) classifyAzError(err error, stderr string) error {
	// az not installed or not on PATH.
	var execErr *exec.Error
	if errors.As(err, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound) {
		return errkit.New("MCP-007",
			"mcp-http: failed to acquire auth token: az CLI is not installed or not on PATH — install the Azure CLI from https://aka.ms/installazurecli and retry")
	}

	// Invalid audience/scope errors can include an "az login" remediation hint;
	// classify them before generic login text so operators fix the config.
	if containsAny(stderr, "AADSTS500011", "AADSTS700011", "AADSTS70011", "AADSTS700016") {
		targetKind, targetValue := p.authTarget()
		return errkit.New("MCP-007",
			fmt.Sprintf("mcp-http: failed to acquire auth token: %s %q is not valid in this tenant — fix transport.auth.%s", targetKind, targetValue, targetKind))
	}

	// Parse stderr for known auth failure patterns.
	if containsAny(stderr, "az login", "please run", "run 'az", "please sign in", "AADSTS700082") {
		return errkit.New("MCP-007",
			"mcp-http: failed to acquire auth token: not authenticated — run 'az login' and retry")
	}

	if containsAny(stderr, "AADSTS65001", "AADSTS65004") {
		targetKind, targetValue := p.authTarget()
		return errkit.New("MCP-007",
			fmt.Sprintf("mcp-http: failed to acquire auth token: no consent for %s %q — grant application consent or run 'az login' with the required target", targetKind, targetValue))
	}

	// Generic failure: include a truncated stderr snippet for diagnostics.
	// Truncated to prevent log flooding and to exclude any partial output.
	snippet := strings.TrimSpace(stderr)
	if len(snippet) > 250 {
		snippet = snippet[:250] + "…"
	}
	if snippet == "" {
		snippet = err.Error()
	}
	return errkit.New("MCP-007",
		fmt.Sprintf("mcp-http: failed to acquire auth token: %s", snippet))
}

// parseAzExpiry parses the token expiry from az's output fields.
// expiresOnTS is a Unix timestamp string (preferred); expiresOn is a
// local-time string (fallback). Returns now+50min if neither can be parsed
// — a safe default well under typical token lifetime (60–75 min).
func parseAzExpiry(expiresOnTS, expiresOn string) time.Time {
	if expiresOnTS != "" {
		if ts, err := strconv.ParseInt(strings.TrimSpace(expiresOnTS), 10, 64); err == nil {
			return time.Unix(ts, 0)
		}
	}
	if expiresOn != "" {
		// az uses "2006-01-02 15:04:05.000000" in local time.
		if t, err := time.ParseInLocation("2006-01-02 15:04:05.000000", strings.TrimSpace(expiresOn), time.Local); err == nil {
			return t
		}
		// Some versions omit sub-seconds.
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", strings.TrimSpace(expiresOn), time.Local); err == nil {
			return t
		}
	}
	return time.Now().Add(50 * time.Minute)
}

// realAzRunner executes az on the real PATH.
func realAzRunner(ctx context.Context, args []string) ([]byte, string, error) {
	azPath, err := exec.LookPath("az")
	if err != nil {
		return nil, "", &exec.Error{Name: "az", Err: exec.ErrNotFound}
	}
	cmd := exec.CommandContext(ctx, azPath, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	return outBuf.Bytes(), errBuf.String(), runErr
}

// containsAny reports whether s contains any of the given substrings
// (case-insensitive).
func containsAny(s string, subs ...string) bool {
	lower := strings.ToLower(s)
	for _, sub := range subs {
		if strings.Contains(lower, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}
