package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
)

// imdsEndpoint is the Azure Instance Metadata Service (IMDS) token endpoint.
// This address is only reachable from an Azure-hosted VM or container.
// On any non-Azure host the HTTP GET will time-out or be refused — context
// cancellation propagated by Token() ensures that does not hang forever.
const imdsEndpoint = "http://169.254.169.254/metadata/identity/oauth2/token"

// imdsAPIVersion is the IMDS API version passed as a query parameter.
const imdsAPIVersion = "2018-02-01"

// imdsTokenResponse is the shape returned by the IMDS token endpoint.
// expires_on and expires_in are both returned; we prefer expires_on (absolute
// Unix timestamp) and fall back to expires_in (seconds from now).
type imdsTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresOn   string `json:"expires_on"` // Unix timestamp string (preferred)
	ExpiresIn   string `json:"expires_in"` // seconds from now (fallback)
	TokenType   string `json:"token_type"`
}

// imdsHTTPClient is the function type for executing IMDS HTTP requests.
// Replaced in tests with a stub; nil means use a real http.Client.
type imdsHTTPClient func(req *http.Request) (*http.Response, error)

// ManagedIdentityAuthProvider acquires bearer tokens from the Azure IMDS
// endpoint. It supports both system-assigned and user-assigned managed
// identities; for user-assigned identities, supply the client ID via
// AuthConfig's optional ClientID (passed as clientID to New).
//
// Caching strategy mirrors AzureCLIAuthProvider:
//   - Token is cached with its expiry parsed from the IMDS response.
//   - Token() returns the cached value while now+5min < expiry.
//   - Token() proactively re-acquires when within 5 min of expiry.
//     If that early re-acquisition fails and the token is still valid,
//     the still-valid cached token is returned rather than failing the call.
//   - Once the token has expired, Token() MUST re-acquire and returns
//     an error on failure.
//   - Invalidate() clears the cache; the next Token() call re-acquires.
//     The HTTP transport calls this on receipt of HTTP 401.
//
// This provider ONLY contacts the IMDS endpoint. It NEVER inspects ambient
// environment variables (e.g. AZURE_FEDERATED_TOKEN_FILE). Federated/workload
// identity is a separate provider deferred to a later phase.
type ManagedIdentityAuthProvider struct {
	resource string // OAuth2 resource URI (from AuthConfig.Scope)
	clientID string // optional; non-empty = user-assigned identity

	httpClient imdsHTTPClient // nil → real http.Client.Do

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// NewManagedIdentityAuthProvider constructs a ManagedIdentityAuthProvider.
// resource is the OAuth2 scope/audience (e.g. "api://icmmcpapi-prod/mcp.tools").
// clientID is optional; pass "" for a system-assigned identity.
func NewManagedIdentityAuthProvider(resource, clientID string) *ManagedIdentityAuthProvider {
	return &ManagedIdentityAuthProvider{
		resource: resource,
		clientID: clientID,
	}
}

// Token returns a valid bearer token for the configured resource.
// It contacts IMDS at most once per token lifetime (minus the 5-minute
// refresh buffer). context.Context deadline and cancellation are propagated
// into the HTTP request, ensuring the call is not left hanging on a
// non-Azure host.
func (p *ManagedIdentityAuthProvider) Token(ctx context.Context) (string, error) {
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
func (p *ManagedIdentityAuthProvider) Invalidate() {
	p.mu.Lock()
	p.token = ""
	p.expiry = time.Time{}
	p.mu.Unlock()
}

// imdsEndpointOverride, when non-empty, replaces imdsEndpoint in acquire().
// It is set only by SetIMDSEndpointForTest; it must never be set in
// production code. The zero value ("") means use the real IMDS endpoint.
var imdsEndpointOverride string

// SetIMDSEndpointForTest redirects IMDS requests to endpoint for the duration
// of a test. It returns a cleanup function that restores the original value;
// callers must invoke it (typically via t.Cleanup). This function exists solely
// to allow cmd/yawr integration tests (which run through the full runRun()
// execution path) to inject a mock IMDS server without changing the managed-
// identity provider's internal construction path.
//
// Must only be called from test code. Must not be called concurrently.
func SetIMDSEndpointForTest(endpoint string) func() {
	prev := imdsEndpointOverride
	imdsEndpointOverride = endpoint
	return func() { imdsEndpointOverride = prev }
}

// acquire contacts IMDS and updates the cache. Caller must hold p.mu.
func (p *ManagedIdentityAuthProvider) acquire(ctx context.Context) (string, error) {
	endpoint := imdsEndpoint
	if imdsEndpointOverride != "" {
		endpoint = imdsEndpointOverride
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", errkit.New("MCP-007",
			fmt.Sprintf("mcp-http: failed to acquire auth token: could not build IMDS request: %v", err))
	}

	q := req.URL.Query()
	q.Set("api-version", imdsAPIVersion)
	q.Set("resource", p.resource)
	if p.clientID != "" {
		q.Set("client_id", p.clientID)
	}
	req.URL.RawQuery = q.Encode()

	// The Metadata: true header is required by IMDS; requests without it
	// are rejected with HTTP 400 as a SSRF mitigation.
	req.Header.Set("Metadata", "true")

	doRequest := p.httpClient
	if doRequest == nil {
		c := &http.Client{}
		doRequest = c.Do
	}

	resp, err := doRequest(req)
	if err != nil {
		// Context cancellation propagates here.
		if ctx.Err() != nil {
			return "", errkit.New("MCP-007",
				fmt.Sprintf("mcp-http: failed to acquire auth token: IMDS request cancelled: %v", ctx.Err()))
		}
		return "", errkit.New("MCP-007",
			"mcp-http: failed to acquire auth token: IMDS endpoint unreachable — is this an Azure-hosted environment?")
	}
	defer resp.Body.Close()

	// Read at most 8 KiB to guard against a runaway response. The token and
	// its metadata are well under this limit; the raw body is never surfaced
	// in error messages (B-24: tokens must not appear in observable surfaces).
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	if err != nil {
		return "", errkit.New("MCP-007",
			"mcp-http: failed to acquire auth token: could not read IMDS response body")
	}

	if resp.StatusCode != http.StatusOK {
		// Do NOT include the body in the error message — on error the body
		// may contain partial JSON with a token field or security-sensitive
		// diagnostic data. Report status code and IMDS URL only.
		return "", errkit.New("MCP-007",
			fmt.Sprintf("mcp-http: failed to acquire auth token: IMDS returned HTTP %d (endpoint: %s)", resp.StatusCode, imdsEndpoint))
	}

	var tokenResp imdsTokenResponse
	if jsonErr := json.Unmarshal(body, &tokenResp); jsonErr != nil {
		return "", errkit.New("MCP-007",
			fmt.Sprintf("mcp-http: failed to acquire auth token: IMDS returned malformed JSON: %v", jsonErr))
	}
	if tokenResp.AccessToken == "" {
		return "", errkit.New("MCP-007",
			"mcp-http: failed to acquire auth token: IMDS response is missing access_token field")
	}

	expiry := parseIMDSExpiry(tokenResp.ExpiresOn, tokenResp.ExpiresIn)
	p.token = tokenResp.AccessToken
	p.expiry = expiry
	return tokenResp.AccessToken, nil
}

// parseIMDSExpiry parses the token expiry from IMDS response fields.
// ExpiresOn is a Unix timestamp string (preferred); ExpiresIn is a seconds
// string (fallback). Returns now+50min if neither can be parsed — a safe
// default well under typical token lifetime (60–75 min).
func parseIMDSExpiry(expiresOn, expiresIn string) time.Time {
	if expiresOn != "" {
		if ts, err := strconv.ParseInt(strings.TrimSpace(expiresOn), 10, 64); err == nil {
			return time.Unix(ts, 0)
		}
	}
	if expiresIn != "" {
		if secs, err := strconv.ParseInt(strings.TrimSpace(expiresIn), 10, 64); err == nil {
			return time.Now().Add(time.Duration(secs) * time.Second)
		}
	}
	return time.Now().Add(50 * time.Minute)
}
