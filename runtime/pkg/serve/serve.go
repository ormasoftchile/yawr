package serve

import (
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// ServerConfig holds all configuration for the yawr serve HTTP server.
type ServerConfig struct {
	// Addr is the TCP address to listen on (default: ":7778").
	Addr string

	// EngineConfig is passed to engine construction.
	EngineConfig engine.EngineConfig

	// Engine optionally supplies a preconstructed engine implementation.
	Engine engine.Engine

	// EngineFactory constructs an engine from the provided EngineConfig.
	EngineFactory func(engine.EngineConfig) engine.Engine

	// Parser parses runbooks for run.start.
	Parser parser.Parser

	// Planner resolves parsed runbooks into execution plans.
	Planner planner.Planner

	// WorkspaceRoot is the root used for per-run package catalog resolution.
	// When empty, the server process working directory is used.
	WorkspaceRoot  string
	PackageMapPath string

	// ProjectRequires are the already-merged project/package-map requires:
	// bindings used as catalog Tier 1 inputs for every served run. The posted
	// runbook's own requires: are merged by the catalog builder per run.
	ProjectRequires []*schema.PackageRequirement

	// ProjectToolPaths are the already-merged project/package-map tool-paths:
	// bindings used as catalog Tier 2 inputs for every served run.
	ProjectToolPaths []string

	// ReadTimeout is the maximum duration for reading request body.
	// Default: 30s.
	ReadTimeout time.Duration

	// WriteTimeout is the maximum duration before timing out response writes.
	// Default: 60s.
	WriteTimeout time.Duration

	// MaxRunAge is how long completed/failed runs remain in the registry.
	// Default: 1h. After this duration, GC removes them.
	MaxRunAge time.Duration

	// WSPingInterval is how often the server pings WebSocket clients.
	// Default: 30s.
	WSPingInterval time.Duration

	// EventBufferSize is the channel buffer for per-client event delivery.
	// Default: 256. Events are dropped for slow consumers.
	EventBufferSize int

	// AllowedOrigins is the list of allowed CORS origins.
	// An empty slice permits all origins (wildcard). A specific list restricts to those origins.
	AllowedOrigins []string

	// BearerToken is the shared secret for simple bearer token authentication.
	// When non-empty, all non-health routes require "Authorization: Bearer <token>".
	// When empty, authentication is disabled (open access).
	// NOTE: This is intended for development/internal deployments only.
	// Mutually exclusive with JWTSecret.
	BearerToken string

	// BearerTokenExpiry is the maximum age of a bearer token.
	// When zero (default), tokens are treated as opaque shared secrets with no expiry check.
	// When JWTSecret is set, this controls the maximum age of the iat claim.
	BearerTokenExpiry time.Duration

	// JWTSecret is the HMAC-SHA256 secret for JWT signature verification.
	// When non-nil, all non-health routes require a valid, signed HS256 JWT.
	// Mutually exclusive with BearerToken.
	JWTSecret []byte

	// RateLimit is the maximum requests per second per IP.
	// Default: 0 (disabled). Typical value: 10-100.
	RateLimit int

	// TrustProxyHeaders controls whether X-Forwarded-For and X-Real-IP headers are trusted
	// for IP extraction in the rate-limit middleware.
	// Enable ONLY when yawr serve is deployed behind a trusted reverse proxy (nginx, Caddy, etc.).
	// When false (default), rate limiting uses RemoteAddr exclusively.
	// WARNING: enabling this on a directly-internet-facing server allows IP spoofing.
	TrustProxyHeaders bool
}

// RunEvent is the wire-level event envelope sent over WS/SSE.
type RunEvent struct {
	Type     string         `json:"type"`
	RunID    string         `json:"runID"`
	Sequence int64          `json:"sequence"`
	TS       string         `json:"ts"`
	Payload  map[string]any `json:"payload"`
}
