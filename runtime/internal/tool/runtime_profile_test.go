package tool

// TestCLI_ProfileEndpointOverride_Reachable is the reachability test for
// ProfileToolOverride.Endpoint: it proves the field changes observable
// runtime behaviour and MUST FAIL if the production read of
// override.Endpoint in DefaultToolRuntime.Invoke is removed.
//
// Convention: "TestCLI_<Feature>_Reachable" per Ken's convention; written
// standalone (not yet using a shared helper) as agreed in David's decision
// record.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// TestCLI_ProfileEndpointOverride_Reachable verifies that a profile's
// per-tool endpoint override actually changes which server receives the
// MCP request. Two in-process servers are started:
//   - defaultSrv: the URL written into the tool definition
//   - overrideSrv: the URL set in the profile's per-tool endpoint
//
// With the profile wired in, all traffic must flow to overrideSrv.
// Removing the effectiveURL read in DefaultToolRuntime.Invoke causes
// traffic to flow to defaultSrv, which then fails the assertion.
func TestCLI_ProfileEndpointOverride_Reachable(t *testing.T) {
	var defaultHits, overrideHits int

	defaultServer := &fakeMCPServer{}
	overrideServer := &fakeMCPServer{}

	defaultSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defaultHits++
		defaultServer.ServeHTTP(w, r)
	}))
	t.Cleanup(defaultSrv.Close)

	overrideSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		overrideHits++
		overrideServer.ServeHTTP(w, r)
	}))
	t.Cleanup(overrideSrv.Close)

	// Tool definition points at defaultSrv. No auth (plain HTTP, no HTTPS
	// required here since we bypass ValidateTransportConfig by constructing
	// ToolDef directly — runtime.go only calls ValidateAuthConfig which does
	// not enforce HTTPS).
	def := toolpkg.ToolDef{
		Name:      "icm",
		Transport: toolpkg.TransportMCPHTTP,
		URL:       defaultSrv.URL,
		Actions: map[string]*toolpkg.ToolAction{
			"get-incident": {Description: "Get an incident"},
		},
	}
	registry := &mapRegistry{defs: map[string]*toolpkg.ToolDef{"icm": &def}}
	rt := NewDefaultToolRuntime(registry)

	// Profile overrides the endpoint to overrideSrv.
	profile := &schema.RuntimeProfile{
		APIVersion: schema.RuntimeProfileAPIVersion,
		ID:         "override-test",
		Context:    schema.ProfileContextCLIOperator,
		Attendance: schema.ProfileAttendanceAttended,
		Tools: map[string]*schema.ProfileToolOverride{
			"icm": {Endpoint: overrideSrv.URL},
		},
	}
	rt.SetProfile(profile)

	// Invoke — the transport is created on first call.
	_, _ = rt.Invoke(context.Background(), "icm", "get-incident", map[string]any{})

	// Reachability assertion: overrideSrv MUST have received requests,
	// defaultSrv MUST NOT. If the production read of override.Endpoint is
	// removed, traffic flows to defaultSrv and this test fails.
	if overrideHits == 0 {
		t.Errorf("overrideSrv received 0 requests — endpoint override was not applied; " +
			"ProfileToolOverride.Endpoint is not being read in DefaultToolRuntime.Invoke")
	}
	if defaultHits > 0 {
		t.Errorf("defaultSrv received %d requests — endpoint override was bypassed; "+
			"traffic should flow exclusively to overrideSrv", defaultHits)
	}
}

// TestRun_Profile_TransportModeRewrite_Rejected verifies that a profile
// attempting to set a transport mode is rejected at parse time (PROF-001),
// before any execution occurs. This is the "profile MUST NOT rewrite
// transport mode" guarantee from the plan.
func TestRun_Profile_TransportModeRewrite_Rejected(t *testing.T) {
	profileYAML := []byte(`apiVersion: yawr.runtime-profile/v1
id: bad-profile
context: cli-operator
attendance: attended
tools:
  icm:
    mode: mcp-http
`)
	_, err := schema.ParseProfileBytes(profileYAML)
	if err == nil {
		t.Fatal("ParseProfileBytes: want error (PROF-001 transport-mode rewrite), got nil")
	}
	t.Logf("correctly rejected (PROF-001): %v", err)
}

// mapRegistry is a minimal ToolRegistry backed by a plain map. Used in tests
// where the full scan-based registry is unnecessary.
type mapRegistry struct {
	defs map[string]*toolpkg.ToolDef
}

func (r *mapRegistry) Lookup(name string) (*toolpkg.ToolDef, bool) {
	d, ok := r.defs[name]
	return d, ok
}

func (r *mapRegistry) All() []toolpkg.ToolDef {
	out := make([]toolpkg.ToolDef, 0, len(r.defs))
	for _, d := range r.defs {
		out = append(out, *d)
	}
	return out
}

func (r *mapRegistry) Register(def toolpkg.ToolDef) error {
	r.defs[def.Name] = &def
	return nil
}
