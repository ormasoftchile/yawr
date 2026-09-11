// Package errkit defines typed YAWR conformance errors.
package errkit

import (
	"fmt"
	"strings"
)

// Classer is implemented by errors that expose a conformance error class.
type Classer interface {
	Class() string
}

// Coder is implemented by errors that expose a conformance error code.
type Coder interface {
	Code() string
}

// Error is a typed YAWR error compatible with errors.Is against sentinels.
type Error struct {
	code    string
	class   string
	message string
	cause   error
}

// New returns an error for code with message.
func New(code, message string) *Error {
	return Wrap(code, message, nil)
}

// Wrap returns an error for code with message and cause.
func Wrap(code, message string, cause error) *Error {
	return &Error{code: code, class: ClassForCode(code), message: message, cause: cause}
}

// Error returns the formatted error message.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.message == "" {
		return e.code
	}
	return fmt.Sprintf("%s: %s", e.code, e.message)
}

// Unwrap returns the wrapped cause, if any.
func (e *Error) Unwrap() error { return e.cause }

// Class returns the conformance error class string.
func (e *Error) Class() string { return e.class }

// Code returns the conformance error code string.
func (e *Error) Code() string { return e.code }

// Is reports whether target has the same conformance code or class.
func (e *Error) Is(target error) bool {
	if e == nil || target == nil {
		return false
	}
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	if t.code != "" {
		return e.code == t.code
	}
	return t.class != "" && e.class == t.class
}

// ClassForCode returns the conformance class for an error code.
func ClassForCode(code string) string {
	switch {
	case code == "GIS-PATH-MISSING":
		return "GIS-PATH"
	case code == "GCP-DEFAULT-SUBTREE":
		return "GCP-DEFAULT"
	case strings.HasPrefix(code, "GCP-RESOLVE-"):
		return "GCP-RESOLVE"
	case strings.HasPrefix(code, "GXL-") || strings.HasPrefix(code, "GIS-"):
		parts := strings.Split(code, "-")
		if len(parts) >= 2 {
			return parts[0] + "-" + parts[1]
		}
	case strings.HasPrefix(code, "GCP-"):
		parts := strings.Split(code, "-")
		if len(parts) >= 2 {
			return parts[0] + "-" + parts[1]
		}
	case strings.HasPrefix(code, "PKG-W"):
		return "PKG-W"
	case strings.HasPrefix(code, "PKG-"):
		return "PKG"
	case strings.HasPrefix(code, "PLAN-"):
		return "PLAN"
	case code == "ENUM-W001":
		return "ENUM-W"
	case strings.HasPrefix(code, "ENUM-W"):
		return "ENUM-W"
	case strings.HasPrefix(code, "ENUM-"):
		return "ENUM"
	case strings.HasPrefix(code, "DINC-W"):
		return "DINC-W"
	case strings.HasPrefix(code, "DINC-"):
		return "DINC"
	case strings.HasPrefix(code, "MCP-"):
		return "MCP"
	}
	return ""
}

// Sentinel returns the sentinel for code, or nil if the code is unknown.
func Sentinel(code string) *Error { return sentinels[code] }

// ClassSentinel returns the sentinel for class, or nil if the class is unknown.
func ClassSentinel(class string) *Error { return classSentinels[class] }

// IsWarning reports whether err is a warning-class advisory rather than a
// fatal error. Warning classes end with "-W" (e.g. "PKG-W", "DINC-W").
// Callers that accumulate []error from pkgcatalog.Build/BindFile MUST
// use this (or equivalent Class()=="PKG-W" filtering) to separate warnings
// from fatal errors before deciding whether to abort: per Barbara's binding
// ruling (TV-PKG-PATH-002), a workspace-level path escaping the workspace
// root is reported via PKG-W003 and MUST NOT cause the operation to fail.
func IsWarning(err error) bool {
	if err == nil {
		return false
	}
	if c, ok := err.(Classer); ok {
		return strings.HasSuffix(c.Class(), "-W")
	}
	return false
}

// SplitWarnings partitions errs into (fatal, warnings) using IsWarning.
func SplitWarnings(errs []error) (fatal []error, warnings []error) {
	for _, e := range errs {
		if IsWarning(e) {
			warnings = append(warnings, e)
		} else {
			fatal = append(fatal, e)
		}
	}
	return fatal, warnings
}

// Codes returns all conformance error codes known to the P1 registry.
func Codes() []string {
	out := make([]string, 0, len(codeOrder))
	out = append(out, codeOrder...)
	return out
}

// Classes returns all conformance error classes known to the P1 registry.
func Classes() []string {
	return []string{"GXL-PARSE", "GXL-TYPE", "GXL-PATH", "GXL-EVAL", "GIS-PARSE", "GIS-PATH", "GIS-TYPE", "GIS-EVAL", "GCP-PARSE", "GCP-RESOLVE", "GCP-DEFAULT", "GCP-TYPE", "GCP-EVAL", "PKG", "PKG-W", "ENUM", "ENUM-W", "DINC", "DINC-W", "MCP"}
}

var (
	// ErrGXLParse001 is the GXL-PARSE-001 sentinel.
	ErrGXLParse001 = &Error{code: "GXL-PARSE-001", class: "GXL-PARSE"}
	// ErrGXLParse002 is the GXL-PARSE-002 sentinel.
	ErrGXLParse002 = &Error{code: "GXL-PARSE-002", class: "GXL-PARSE"}
	// ErrGXLParse003 is the GXL-PARSE-003 sentinel.
	ErrGXLParse003 = &Error{code: "GXL-PARSE-003", class: "GXL-PARSE"}
	// ErrGXLParse004 is the GXL-PARSE-004 sentinel.
	ErrGXLParse004 = &Error{code: "GXL-PARSE-004", class: "GXL-PARSE"}
	// ErrGXLParse005 is the GXL-PARSE-005 sentinel.
	ErrGXLParse005 = &Error{code: "GXL-PARSE-005", class: "GXL-PARSE"}
	// ErrGXLParse006 is the GXL-PARSE-006 sentinel.
	ErrGXLParse006 = &Error{code: "GXL-PARSE-006", class: "GXL-PARSE"}
	// ErrGXLParse007 is the GXL-PARSE-007 sentinel.
	ErrGXLParse007 = &Error{code: "GXL-PARSE-007", class: "GXL-PARSE"}
	// ErrGXLParse008 is the GXL-PARSE-008 sentinel.
	ErrGXLParse008 = &Error{code: "GXL-PARSE-008", class: "GXL-PARSE"}
	// ErrGXLParse009 is the GXL-PARSE-009 sentinel.
	ErrGXLParse009 = &Error{code: "GXL-PARSE-009", class: "GXL-PARSE"}
	// ErrGXLParse010 is the GXL-PARSE-010 sentinel.
	ErrGXLParse010 = &Error{code: "GXL-PARSE-010", class: "GXL-PARSE"}
	// ErrGXLPath001 is the GXL-PATH-001 sentinel.
	ErrGXLPath001 = &Error{code: "GXL-PATH-001", class: "GXL-PATH"}
	// ErrGXLPath002 is the GXL-PATH-002 sentinel.
	ErrGXLPath002 = &Error{code: "GXL-PATH-002", class: "GXL-PATH"}
	// ErrGXLPath003 is the GXL-PATH-003 sentinel.
	ErrGXLPath003 = &Error{code: "GXL-PATH-003", class: "GXL-PATH"}
	// ErrGXLPath004 is the GXL-PATH-004 sentinel.
	ErrGXLPath004 = &Error{code: "GXL-PATH-004", class: "GXL-PATH"}
	// ErrGXLType001 is the GXL-TYPE-001 sentinel.
	ErrGXLType001 = &Error{code: "GXL-TYPE-001", class: "GXL-TYPE"}
	// ErrGXLType002 is the GXL-TYPE-002 sentinel.
	ErrGXLType002 = &Error{code: "GXL-TYPE-002", class: "GXL-TYPE"}
	// ErrGXLType003 is the GXL-TYPE-003 sentinel.
	ErrGXLType003 = &Error{code: "GXL-TYPE-003", class: "GXL-TYPE"}
	// ErrGXLType004 is the GXL-TYPE-004 sentinel.
	ErrGXLType004 = &Error{code: "GXL-TYPE-004", class: "GXL-TYPE"}
	// ErrGXLType005 is the GXL-TYPE-005 sentinel.
	ErrGXLType005 = &Error{code: "GXL-TYPE-005", class: "GXL-TYPE"}
	// ErrGXLEval001 is the GXL-EVAL-001 sentinel.
	ErrGXLEval001 = &Error{code: "GXL-EVAL-001", class: "GXL-EVAL"}
	// ErrGXLEval002 is the GXL-EVAL-002 sentinel.
	ErrGXLEval002 = &Error{code: "GXL-EVAL-002", class: "GXL-EVAL"}
	// ErrGXLEval003 is the GXL-EVAL-003 sentinel.
	ErrGXLEval003 = &Error{code: "GXL-EVAL-003", class: "GXL-EVAL"}
	// ErrGXLEval004 is the GXL-EVAL-004 sentinel.
	ErrGXLEval004 = &Error{code: "GXL-EVAL-004", class: "GXL-EVAL"}
	// ErrGISParse003 is the GIS-PARSE-003 sentinel.
	ErrGISParse003 = &Error{code: "GIS-PARSE-003", class: "GIS-PARSE"}
	// ErrGISPathMissing is the GIS-PATH-MISSING sentinel.
	ErrGISPathMissing = &Error{code: "GIS-PATH-MISSING", class: "GIS-PATH"}
	// ErrGCPParse001 is the GCP-PARSE-001 sentinel.
	ErrGCPParse001 = &Error{code: "GCP-PARSE-001", class: "GCP-PARSE"}
	// ErrGCPParse002 is the GCP-PARSE-002 sentinel.
	ErrGCPParse002 = &Error{code: "GCP-PARSE-002", class: "GCP-PARSE"}
	// ErrGCPParse003 is the GCP-PARSE-003 sentinel.
	ErrGCPParse003 = &Error{code: "GCP-PARSE-003", class: "GCP-PARSE"}
	// ErrGCPParse004 is the GCP-PARSE-004 sentinel.
	ErrGCPParse004 = &Error{code: "GCP-PARSE-004", class: "GCP-PARSE"}
	// ErrGCPParse005 is the GCP-PARSE-005 sentinel.
	ErrGCPParse005 = &Error{code: "GCP-PARSE-005", class: "GCP-PARSE"}
	// ErrGCPParse006 is the GCP-PARSE-006 sentinel.
	ErrGCPParse006 = &Error{code: "GCP-PARSE-006", class: "GCP-PARSE"}
	// ErrGCPResolve001 is the GCP-RESOLVE-001 sentinel.
	ErrGCPResolve001 = &Error{code: "GCP-RESOLVE-001", class: "GCP-RESOLVE"}
	// ErrGCPResolve002 is the GCP-RESOLVE-002 sentinel.
	ErrGCPResolve002 = &Error{code: "GCP-RESOLVE-002", class: "GCP-RESOLVE"}
	// ErrGCPResolve003 is the GCP-RESOLVE-003 sentinel.
	ErrGCPResolve003 = &Error{code: "GCP-RESOLVE-003", class: "GCP-RESOLVE"}
	// ErrGCPResolve004 is the GCP-RESOLVE-004 sentinel.
	ErrGCPResolve004 = &Error{code: "GCP-RESOLVE-004", class: "GCP-RESOLVE"}
	// ErrGCPDefaultSubtree is the GCP-DEFAULT-SUBTREE sentinel.
	ErrGCPDefaultSubtree = &Error{code: "GCP-DEFAULT-SUBTREE", class: "GCP-DEFAULT"}
	// ErrGCPType001 is the GCP-TYPE-001 sentinel.
	ErrGCPType001 = &Error{code: "GCP-TYPE-001", class: "GCP-TYPE"}

	// ErrPKG001 is the PKG-001 sentinel (required package not resolvable).
	ErrPKG001 = &Error{code: "PKG-001", class: "PKG"}
	// ErrPKG002 is the PKG-002 sentinel (version constraint unsatisfied / empty intersection).
	ErrPKG002 = &Error{code: "PKG-002", class: "PKG"}
	// ErrPKG003 is the PKG-003 sentinel (malformed version constraint).
	ErrPKG003 = &Error{code: "PKG-003", class: "PKG"}
	// ErrPKG004 is the PKG-004 sentinel (package manifest fails schema validation).
	ErrPKG004 = &Error{code: "PKG-004", class: "PKG"}
	// ErrPKG005 is the PKG-005 sentinel (duplicate package name in one requires: list).
	ErrPKG005 = &Error{code: "PKG-005", class: "PKG"}
	// ErrPKG006 is the PKG-006 sentinel (same-tier tool name collision).
	ErrPKG006 = &Error{code: "PKG-006", class: "PKG"}
	// ErrPKG007 is the PKG-007 sentinel (unsafe or escaping path).
	ErrPKG007 = &Error{code: "PKG-007", class: "PKG"}
	// ErrPKG008 is the PKG-008 sentinel (symlink/junction escape, cycle, or hop limit).
	ErrPKG008 = &Error{code: "PKG-008", class: "PKG"}
	// ErrPKG009 is the PKG-009 sentinel (package or catalog digest mismatch).
	ErrPKG009 = &Error{code: "PKG-009", class: "PKG"}
	// ErrPKG010 is the PKG-010 sentinel (export references a missing file).
	ErrPKG010 = &Error{code: "PKG-010", class: "PKG"}
	// ErrPKG011 is the PKG-011 sentinel (referenced tool is not exported by the package).
	ErrPKG011 = &Error{code: "PKG-011", class: "PKG"}
	// ErrPKG012 is the PKG-012 sentinel (action not declared by the resolved tool).
	ErrPKG012 = &Error{code: "PKG-012", class: "PKG"}
	// ErrPKG013 is the PKG-013 sentinel (substitute runbook input/output signature mismatch).
	ErrPKG013 = &Error{code: "PKG-013", class: "PKG"}
	// ErrPKG014 is the PKG-014 sentinel (substitute would widen effective governance).
	ErrPKG014 = &Error{code: "PKG-014", class: "PKG"}
	// ErrPKG015 is the PKG-015 sentinel (substitution cycle).
	ErrPKG015 = &Error{code: "PKG-015", class: "PKG"}
	// ErrPKG016 is the PKG-016 sentinel (non-empty dependencies:).
	ErrPKG016 = &Error{code: "PKG-016", class: "PKG"}
	// ErrPKG017 is the PKG-017 sentinel (late package binding after catalog freeze).
	ErrPKG017 = &Error{code: "PKG-017", class: "PKG"}
	// ErrPKG018 is the PKG-018 sentinel (reserved name, case/NFC-only collision, trailing dot/space).
	ErrPKG018 = &Error{code: "PKG-018", class: "PKG"}
	// ErrPKG020 is the PKG-020 sentinel (toolPackages: key present).
	ErrPKG020 = &Error{code: "PKG-020", class: "PKG"}
	// ErrPKG021 is the PKG-021 sentinel (toolRefs[].alias present).
	ErrPKG021 = &Error{code: "PKG-021", class: "PKG"}
	// ErrPKG022 is the PKG-022 sentinel (undeclared cross-tier shadowing of a bare tool name).
	ErrPKG022 = &Error{code: "PKG-022", class: "PKG"}
	// ErrPKG023 is the PKG-023 sentinel (export id != tool definition name).
	ErrPKG023 = &Error{code: "PKG-023", class: "PKG"}
	// ErrPKG024 is the PKG-024 sentinel (runbook requires[].path conflicts with project binding).
	ErrPKG024 = &Error{code: "PKG-024", class: "PKG"}
	// ErrPKG025 is the PKG-025 sentinel (version is not strict SemVer 2.0.0).
	ErrPKG025 = &Error{code: "PKG-025", class: "PKG"}
	// ErrPKG026 is the PKG-026 sentinel (substitute declares from: prompt or from: env).
	ErrPKG026 = &Error{code: "PKG-026", class: "PKG"}
	// ErrPKG027 is the PKG-027 sentinel (declared substitute output not produced on all terminal paths).
	ErrPKG027 = &Error{code: "PKG-027", class: "PKG"}
	// ErrPKG028 is the PKG-028 sentinel (substitution depth > 4).
	ErrPKG028 = &Error{code: "PKG-028", class: "PKG"}
	// ErrPKG029 is the PKG-029 sentinel (toolRefs[].package and .path both present).
	ErrPKG029 = &Error{code: "PKG-029", class: "PKG"}
	// ErrPKG030 is the PKG-030 sentinel (name resolves to no catalog entry at all).
	ErrPKG030 = &Error{code: "PKG-030", class: "PKG"}
	// ErrPKGW003 is the PKG-W003 sentinel (package root resolves outside the workspace).
	ErrPKGW003 = &Error{code: "PKG-W003", class: "PKG-W"}
	// ErrPKGW004 is the PKG-W004 sentinel (tool action has no declared classification).
	ErrPKGW004 = &Error{code: "PKG-W004", class: "PKG-W"}

	// ErrPLAN010 is the PLAN-010 sentinel (step.tool.name has no toolRefs
	// binding in the declaring runbook file / no catalog entry resolves
	// the tool name at all; also used for AllowedEnvironments context
	// mismatch — the tool exists but is not configured for this profile).
	ErrPLAN010 = &Error{code: "PLAN-010", class: "PLAN"}

	// ErrPLAN011 is the PLAN-011 sentinel (profile declares
	// attendance: attended but the execution context is structurally
	// unattended — no interactive channel is available).
	ErrPLAN011 = &Error{code: "PLAN-011", class: "PLAN"}

	// ErrPLAN012 is the PLAN-012 sentinel (test-context profile binds a
	// tool whose transport is not permitted in test context: mcp-http is
	// never allowed; mcp subprocess requires explicit
	// transport.allow_subprocess_in_test: true).
	ErrPLAN012 = &Error{code: "PLAN-012", class: "PLAN"}

	// ErrENUM001 is the ENUM-001 sentinel (enum on a non-string declaration, incl. secret).
	ErrENUM001 = &Error{code: "ENUM-001", class: "ENUM"}
	// ErrENUM002 is the ENUM-002 sentinel (enum malformed: not a sequence, empty, or a non-string-resolving/tagged/collection item).
	ErrENUM002 = &Error{code: "ENUM-002", class: "ENUM"}
	// ErrENUM003 is the ENUM-003 sentinel (member is empty, whitespace-only, or has leading/trailing whitespace).
	ErrENUM003 = &Error{code: "ENUM-003", class: "ENUM"}
	// ErrENUM004 is the ENUM-004 sentinel (duplicate members after NFC normalisation).
	ErrENUM004 = &Error{code: "ENUM-004", class: "ENUM"}
	// ErrENUM005 is the ENUM-005 sentinel (member invalid UTF-8, not NFC, or contains control/bidi-format characters).
	ErrENUM005 = &Error{code: "ENUM-005", class: "ENUM"}
	// ErrENUM006 is the ENUM-006 sentinel (default is not a member).
	ErrENUM006 = &Error{code: "ENUM-006", class: "ENUM"}
	// ErrENUM007 is the ENUM-007 sentinel (statically-known literal bound value is not a member, plan time).
	ErrENUM007 = &Error{code: "ENUM-007", class: "ENUM"}
	// ErrENUM008 is the ENUM-008 sentinel (materialised bound value is not a member, runtime).
	ErrENUM008 = &Error{code: "ENUM-008", class: "ENUM"}
	// ErrENUM009 is the ENUM-009 sentinel (declared output value is not a member at production time).
	ErrENUM009 = &Error{code: "ENUM-009", class: "ENUM"}
	// ErrENUMW001 is the ENUM-W001 sentinel (case-only-distinct members; warning, non-fatal).
	ErrENUMW001 = &Error{code: "ENUM-W001", class: "ENUM-W"}

	// Dynamic Include error codes (DINC-001 through DINC-012 and DINC-W001).

	// ErrDINC001 is the DINC-001 sentinel (rendered runbook_ref fails syntactic validation).
	ErrDINC001 = &Error{code: "DINC-001", class: "DINC"}
	// ErrDINC002 is the DINC-002 sentinel (rendered ref not found in catalog).
	ErrDINC002 = &Error{code: "DINC-002", class: "DINC"}
	// ErrDINC003 is the DINC-003 sentinel (bare id is ambiguous in catalog).
	ErrDINC003 = &Error{code: "DINC-003", class: "DINC"}
	// ErrDINC004 is the DINC-004 sentinel (runtime include cycle detected).
	ErrDINC004 = &Error{code: "DINC-004", class: "DINC"}
	// ErrDINC005 is the DINC-005 sentinel (runtime max include depth exceeded).
	ErrDINC005 = &Error{code: "DINC-005", class: "DINC"}
	// ErrDINC006 is the DINC-006 sentinel (required child input not provided).
	ErrDINC006 = &Error{code: "DINC-006", class: "DINC"}
	// ErrDINCW007 is the DINC-W007 sentinel (with: key not declared in child inputs — warning per B-15).
	ErrDINCW007 = &Error{code: "DINC-W007", class: "DINC-W"}
	// ErrDINC008 is the DINC-008 sentinel (with: value violates child input enum constraint).
	ErrDINC008 = &Error{code: "DINC-008", class: "DINC"}
	// ErrDINC009 is the DINC-009 sentinel (with: value type mismatch with child input type).
	ErrDINC009 = &Error{code: "DINC-009", class: "DINC"}
	// ErrDINC010 is the DINC-010 sentinel (child requires package not in frozen catalog).
	ErrDINC010 = &Error{code: "DINC-010", class: "DINC"}
	// ErrDINC011 is the DINC-011 sentinel (child tool name not resolvable in frozen catalog).
	ErrDINC011 = &Error{code: "DINC-011", class: "DINC"}
	// ErrDINC012 is the DINC-012 sentinel (resume drift: dynamic include pin digest mismatch).
	ErrDINC012 = &Error{code: "DINC-012", class: "DINC"}
	// ErrDINC013 is the DINC-013 sentinel (catalog entry resolved but file missing/unreadable from disk).
	ErrDINC013 = &Error{code: "DINC-013", class: "DINC"}
	// ErrDINCW001 is the DINC-W001 sentinel (child governance would widen parent; silently restricted).
	ErrDINCW001 = &Error{code: "DINC-W001", class: "DINC-W"}

	// MCP HTTP transport error codes (MCP-001 through MCP-009). All are fatal.

	// ErrMCP001 is the MCP-001 sentinel (transport.url must use https://).
	ErrMCP001 = &Error{code: "MCP-001", class: "MCP"}
	// ErrMCP002 is the MCP-002 sentinel (auth.provider is not a recognized provider name).
	ErrMCP002 = &Error{code: "MCP-002", class: "MCP"}
	// ErrMCP003 is the MCP-003 sentinel (server rejected MCP protocol version).
	ErrMCP003 = &Error{code: "MCP-003", class: "MCP"}
	// ErrMCP004 is the MCP-004 sentinel (session expired; re-initialization failed).
	ErrMCP004 = &Error{code: "MCP-004", class: "MCP"}
	// ErrMCP005 is the MCP-005 sentinel (unexpected response content-type).
	ErrMCP005 = &Error{code: "MCP-005", class: "MCP"}
	// ErrMCP006 is the MCP-006 sentinel (SSE stream closed without response for request id).
	ErrMCP006 = &Error{code: "MCP-006", class: "MCP"}
	// ErrMCP007 is the MCP-007 sentinel (failed to acquire auth token).
	ErrMCP007 = &Error{code: "MCP-007", class: "MCP"}
	// ErrMCP008 is the MCP-008 sentinel (HTTP transport error: connection refused, TLS failure, timeout).
	ErrMCP008 = &Error{code: "MCP-008", class: "MCP"}
	// ErrMCP009 is the MCP-009 sentinel (JSON-RPC error response from server).
	ErrMCP009 = &Error{code: "MCP-009", class: "MCP"}
	// ErrMCP010 is the MCP-010 sentinel (auth configured but allowed_hosts is absent — B-32).
	ErrMCP010 = &Error{code: "MCP-010", class: "MCP"}
	// ErrMCP011 is the MCP-011 sentinel (url host is not in auth.allowed_hosts — static mismatch, B-32).
	ErrMCP011 = &Error{code: "MCP-011", class: "MCP"}
	// ErrMCP012 is the MCP-012 sentinel (runtime: request host not in allowed_hosts — token not
	// attached and request blocked; fatal per B-32 revised ruling). Emitted by TokenGate.AttachToken
	// when a redirect or programmatic URL diverges from the statically validated URL.
	// Static check (MCP-011) already catches the common case at parse time; MCP-012 is the
	// runtime enforcement that closes the redirect path.
	ErrMCP012 = &Error{code: "MCP-012", class: "MCP"}
	// ErrMCP013 is the MCP-013 sentinel (redirect blocked on authenticated request).
	// Redirect-following is disabled outright for authenticated MCP HTTP requests
	// (http.ErrUseLastResponse); a server that returns a redirect receives no token and
	// the request is failed. Update url: to the final endpoint instead.
	ErrMCP013 = &Error{code: "MCP-013", class: "MCP"}

	// ErrUnknownVariable aliases GXL-PATH-001.
	ErrUnknownVariable = ErrGXLPath001
	// ErrTypeMismatch aliases GXL-TYPE-001.
	ErrTypeMismatch = ErrGXLType001
	// ErrParseInvalidEscape aliases GXL-PARSE-004.
	ErrParseInvalidEscape = ErrGXLParse004
)

var codeOrder = []string{
	"GCP-DEFAULT-SUBTREE", "GCP-PARSE-001", "GCP-PARSE-002", "GCP-PARSE-003", "GCP-PARSE-004", "GCP-PARSE-005", "GCP-PARSE-006", "GCP-RESOLVE-001", "GCP-RESOLVE-002", "GCP-RESOLVE-003", "GCP-RESOLVE-004", "GCP-TYPE-001",
	"GIS-PARSE-003", "GIS-PATH-MISSING",
	"GXL-EVAL-001", "GXL-EVAL-002", "GXL-EVAL-003", "GXL-EVAL-004", "GXL-PARSE-001", "GXL-PARSE-002", "GXL-PARSE-003", "GXL-PARSE-004", "GXL-PARSE-005", "GXL-PARSE-006", "GXL-PARSE-007", "GXL-PARSE-008", "GXL-PARSE-009", "GXL-PARSE-010", "GXL-PATH-001", "GXL-PATH-002", "GXL-PATH-003", "GXL-PATH-004", "GXL-TYPE-001", "GXL-TYPE-002", "GXL-TYPE-003", "GXL-TYPE-004", "GXL-TYPE-005",
	"PKG-001", "PKG-002", "PKG-003", "PKG-004", "PKG-005", "PKG-006", "PKG-007", "PKG-008", "PKG-009", "PKG-010",
	"PKG-011", "PKG-012", "PKG-013", "PKG-014", "PKG-015", "PKG-016", "PKG-017", "PKG-018", "PKG-020", "PKG-021",
	"PKG-022", "PKG-023", "PKG-024", "PKG-025", "PKG-026", "PKG-027", "PKG-028", "PKG-029", "PKG-030",
	"PKG-W003", "PKG-W004",
	"PLAN-010", "PLAN-011", "PLAN-012",
	"ENUM-001", "ENUM-002", "ENUM-003", "ENUM-004", "ENUM-005", "ENUM-006", "ENUM-007", "ENUM-008", "ENUM-009", "ENUM-W001",
	"DINC-001", "DINC-002", "DINC-003", "DINC-004", "DINC-005", "DINC-006", "DINC-008", "DINC-009", "DINC-010", "DINC-011", "DINC-012", "DINC-013", "DINC-W001", "DINC-W007",
	"MCP-001", "MCP-002", "MCP-003", "MCP-004", "MCP-005", "MCP-006", "MCP-007", "MCP-008", "MCP-009", "MCP-010", "MCP-011", "MCP-012", "MCP-013",
}

var classSentinels = map[string]*Error{
	"GXL-PARSE": {class: "GXL-PARSE"}, "GXL-TYPE": {class: "GXL-TYPE"}, "GXL-PATH": {class: "GXL-PATH"}, "GXL-EVAL": {class: "GXL-EVAL"},
	"GIS-PARSE": {class: "GIS-PARSE"}, "GIS-PATH": {class: "GIS-PATH"}, "GIS-TYPE": {class: "GIS-TYPE"}, "GIS-EVAL": {class: "GIS-EVAL"},
	"GCP-PARSE": {class: "GCP-PARSE"}, "GCP-RESOLVE": {class: "GCP-RESOLVE"}, "GCP-DEFAULT": {class: "GCP-DEFAULT"}, "GCP-TYPE": {class: "GCP-TYPE"}, "GCP-EVAL": {class: "GCP-EVAL"},
	"PKG": {class: "PKG"}, "PKG-W": {class: "PKG-W"}, "ENUM": {class: "ENUM"}, "ENUM-W": {class: "ENUM-W"},
	"DINC": {class: "DINC"}, "DINC-W": {class: "DINC-W"},
	"MCP": {class: "MCP"},
}

var sentinels = map[string]*Error{
	"GXL-PARSE-001": ErrGXLParse001, "GXL-PARSE-002": ErrGXLParse002, "GXL-PARSE-003": ErrGXLParse003, "GXL-PARSE-004": ErrGXLParse004, "GXL-PARSE-005": ErrGXLParse005, "GXL-PARSE-006": ErrGXLParse006, "GXL-PARSE-007": ErrGXLParse007, "GXL-PARSE-008": ErrGXLParse008, "GXL-PARSE-009": ErrGXLParse009, "GXL-PARSE-010": ErrGXLParse010,
	"GXL-PATH-001": ErrGXLPath001, "GXL-PATH-002": ErrGXLPath002, "GXL-PATH-003": ErrGXLPath003, "GXL-PATH-004": ErrGXLPath004,
	"GXL-TYPE-001": ErrGXLType001, "GXL-TYPE-002": ErrGXLType002, "GXL-TYPE-003": ErrGXLType003, "GXL-TYPE-004": ErrGXLType004, "GXL-TYPE-005": ErrGXLType005,
	"GXL-EVAL-001": ErrGXLEval001, "GXL-EVAL-002": ErrGXLEval002, "GXL-EVAL-003": ErrGXLEval003, "GXL-EVAL-004": ErrGXLEval004,
	"GIS-PARSE-003": ErrGISParse003, "GIS-PATH-MISSING": ErrGISPathMissing,
	"GCP-PARSE-001": ErrGCPParse001, "GCP-PARSE-002": ErrGCPParse002, "GCP-PARSE-003": ErrGCPParse003, "GCP-PARSE-004": ErrGCPParse004, "GCP-PARSE-005": ErrGCPParse005, "GCP-PARSE-006": ErrGCPParse006,
	"GCP-RESOLVE-001": ErrGCPResolve001, "GCP-RESOLVE-002": ErrGCPResolve002, "GCP-RESOLVE-003": ErrGCPResolve003, "GCP-RESOLVE-004": ErrGCPResolve004, "GCP-DEFAULT-SUBTREE": ErrGCPDefaultSubtree, "GCP-TYPE-001": ErrGCPType001,
	"PKG-001": ErrPKG001, "PKG-002": ErrPKG002, "PKG-003": ErrPKG003, "PKG-004": ErrPKG004, "PKG-005": ErrPKG005,
	"PKG-006": ErrPKG006, "PKG-007": ErrPKG007, "PKG-008": ErrPKG008, "PKG-009": ErrPKG009, "PKG-010": ErrPKG010,
	"PKG-011": ErrPKG011, "PKG-012": ErrPKG012, "PKG-013": ErrPKG013, "PKG-014": ErrPKG014, "PKG-015": ErrPKG015,
	"PKG-016": ErrPKG016, "PKG-017": ErrPKG017, "PKG-018": ErrPKG018, "PKG-020": ErrPKG020, "PKG-021": ErrPKG021,
	"PKG-022": ErrPKG022, "PKG-023": ErrPKG023, "PKG-024": ErrPKG024, "PKG-025": ErrPKG025, "PKG-026": ErrPKG026,
	"PKG-027": ErrPKG027, "PKG-028": ErrPKG028, "PKG-029": ErrPKG029, "PKG-030": ErrPKG030,
	"PKG-W003": ErrPKGW003, "PKG-W004": ErrPKGW004,
	"PLAN-010": ErrPLAN010, "PLAN-011": ErrPLAN011, "PLAN-012": ErrPLAN012,
	"ENUM-001": ErrENUM001, "ENUM-002": ErrENUM002, "ENUM-003": ErrENUM003, "ENUM-004": ErrENUM004, "ENUM-005": ErrENUM005,
	"ENUM-006": ErrENUM006, "ENUM-007": ErrENUM007, "ENUM-008": ErrENUM008, "ENUM-009": ErrENUM009, "ENUM-W001": ErrENUMW001,
	"DINC-001": ErrDINC001, "DINC-002": ErrDINC002, "DINC-003": ErrDINC003, "DINC-004": ErrDINC004,
	"DINC-005": ErrDINC005, "DINC-006": ErrDINC006, "DINC-008": ErrDINC008,
	"DINC-009": ErrDINC009, "DINC-010": ErrDINC010, "DINC-011": ErrDINC011, "DINC-012": ErrDINC012,
	"DINC-013":  ErrDINC013,
	"DINC-W001": ErrDINCW001, "DINC-W007": ErrDINCW007,
	"MCP-001": ErrMCP001, "MCP-002": ErrMCP002, "MCP-003": ErrMCP003, "MCP-004": ErrMCP004, "MCP-005": ErrMCP005,
	"MCP-006": ErrMCP006, "MCP-007": ErrMCP007, "MCP-008": ErrMCP008, "MCP-009": ErrMCP009,
	"MCP-010": ErrMCP010, "MCP-011": ErrMCP011, "MCP-012": ErrMCP012, "MCP-013": ErrMCP013,
}
