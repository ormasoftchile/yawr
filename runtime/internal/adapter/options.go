package adapter

import (
	"io"

	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalroutetest "github.com/ormasoftchile/yawr/runtime/internal/routetest"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/hostaction"
	"github.com/ormasoftchile/yawr/runtime/pkg/input"
	parserpkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

const (
	defaultMode           = "real"
	defaultTraceDir       = "./traces"
	defaultRunDir         = ".runbook/runs"
	defaultInputEnvPrefix = "YAWR_"
	defaultToolScanDir    = "."
)

// WireOptions holds configuration for wiring real components.
type WireOptions struct {
	Mode               string                       // "real", "dry-run", "replay"
	TraceDir           string                       // directory for trace JSONL files
	TraceFile          string                       // explicit trace file (overrides TraceDir)
	RunDir             string                       // base directory for run state, attachments, snapshots
	InputEnvPrefix     string                       // env var prefix for InputProvider chain
	WebhookAddr        string                       // for EventDispatcher webhook listener
	TTYOutput          bool                         // true for yawr run, false for yawr serve
	Output             io.Writer                    // optional executor display output; nil preserves the default
	ToolScanDir        string                       // directory to scan for .tool.yaml files
	ExcludeTestTools   bool                         // omit test fixture trees from the tool registry
	VaultAddr          string                       // optional vault address
	ScenarioFile       string                       // scenario file for replay mode
	RouteTestScheduler *internalroutetest.Scheduler // exact zero-dispatch responses for route-test mode

	// Attended, when non-nil, overrides TTYOutput for approval gate selection only.
	// true = terminal approval gate; false = NoOp approval gate.
	// nil = fall back to TTYOutput (no regression).
	// This decouples approval attendance from TTY input/prompt configuration.
	Attended *bool

	// ApprovalGateOverride, when non-nil, replaces terminal/no-op approval
	// selection. Interactive transports use this to keep approval on their
	// structured wire rather than reading human text from stdin.
	ApprovalGateOverride governance.ApprovalGate

	// PromptProviderOverride, when non-nil, replaces the default
	// terminal-backed PromptProvider. Used by `yawr serve` to install
	// the HTTP PromptBroker before executors are constructed.
	PromptProviderOverride input.PromptProvider

	// HostActionProvider receives typed host_action requests when this
	// runtime has a capable host integration. Nil safely blocks dispatch.
	HostActionProvider hostaction.Provider

	// LazyRunbookLoader, when non-nil, lets the IncludeExecutor
	// materialize includes that the planner deferred (expand=lazy) at
	// run time. May be left nil for callers that never use lazy
	// expansion; in that case, encountering a deferred include at
	// run time produces a clear step-failure error.
	LazyRunbookLoader internalexecutor.LazyRunbookLoader

	// SubstitutionParser, when non-nil, enables the tool executor to run
	// execute.kind: runbook (substituted) actions (pkg/pkgsubst.Plan). May
	// be left nil for callers that never wire package/tool substitution;
	// in that case a substituted action encountered at run time produces
	// a clear step-failure error.
	SubstitutionParser parserpkg.Parser

	// DynamicIncludeResolver resolves dynamic include runbook_ref values at
	// execution time against the frozen catalog. May be left nil for callers
	// that do not use dynamic includes; encountering one at run time then
	// produces a clear step-failure error. In cmd/yawr/run.go this is wired
	// as a ResolverProxy that is populated after Phase C (catalog build)
	// completes so the frozen catalog is reused without rebuilding.
	DynamicIncludeResolver internalexecutor.DynamicIncludeResolver

	// PinRecorder is called after each successful dynamic include resolution
	// so the engine can append the pin to PlanMetadata.DynamicIncludes for
	// replay/resume determinism. Nil is safe: pinning is skipped.
	PinRecorder internalexecutor.PinRecorder

	// MaxIncludeDepth caps the runtime dynamic include depth (0 = default 10).
	MaxIncludeDepth int

	// Profile is the runtime profile to apply during execution. When non-nil,
	// per-tool endpoint and provider overrides from the profile are applied to
	// mcp-http transports when they are first constructed. Nil profile = no
	// overrides (existing behavior preserved). Must be set before the first
	// tool invocation; BuildEngineConfig passes it to DefaultToolRuntime via
	// SetProfile before returning.
	Profile *schema.RuntimeProfile

	// ExtraToolScanPaths, when non-empty, are additional directories scanned
	// for .tool.yaml files after ToolScanDir. Tools found in these paths
	// override any same-named tool found in the base ToolScanDir scan
	// (last-write-wins via OverlayRegistry.Override). Used by yawr serve
	// --package-map to give package-map tool-paths precedence over the
	// workspace scan, so scan order does not decide which definition binds.
	ExtraToolScanPaths []string

	// OTel options — both optional; if neither is set, noop tracer is used.
	OTelEndpoint    string // OTLP gRPC endpoint (e.g. "http://localhost:4317")
	OTelServiceName string // service.name attribute for OTel spans
	OTelStdout      bool   // write spans to stderr as JSON (for debugging)
}

func (o WireOptions) withDefaults() WireOptions {
	if o.Mode == "" {
		o.Mode = defaultMode
	}
	if o.TraceDir == "" {
		o.TraceDir = defaultTraceDir
	}
	if o.RunDir == "" {
		o.RunDir = defaultRunDir
	}
	if o.InputEnvPrefix == "" {
		o.InputEnvPrefix = defaultInputEnvPrefix
	}
	if o.ToolScanDir == "" {
		o.ToolScanDir = defaultToolScanDir
	}
	if o.OTelServiceName == "" {
		o.OTelServiceName = "yawr"
	}
	return o
}
