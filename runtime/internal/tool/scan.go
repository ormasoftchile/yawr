package tool

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// ScanDir discovers .tool.yaml files and returns runtime tool definitions.
func ScanDir(dir string) ([]toolpkg.ToolDef, error) {
	scanned, err := scanSchemaDirWithPaths(dir, false)
	if err != nil {
		return nil, err
	}
	return runtimeDefsFromScanned(scanned)
}

// ScanDirWithoutTests discovers runtime tool definitions while excluding test
// fixture trees. It is used by the long-lived serve process so test doubles
// cannot shadow production tools with the same logical name.
func ScanDirWithoutTests(dir string) ([]toolpkg.ToolDef, error) {
	scanned, err := scanSchemaDirWithPaths(dir, true)
	if err != nil {
		return nil, err
	}
	return runtimeDefsFromScanned(scanned)
}

// runtimeDefsFromScanned converts scanned (schema def + source path) pairs to
// runtime tool definitions and stamps each with the source provenance
// (SourcePath / PackageRoot / PackageName) required to resolve a substituted
// action's execute.path. Without this, a scanned execute.kind: runbook tool
// would reach the executor with an empty PackageRoot and every
// package-internal "../" path would fail pkgpath containment as PKG-007
// (path "..." resolves outside its containment root ""). pkg/pkgcatalog
// stamps the identical provenance from requires: resolution; this is the
// scan-side (yawr serve) equivalent so both binding paths agree.
func runtimeDefsFromScanned(scanned []scannedToolDef) ([]toolpkg.ToolDef, error) {
	out := make([]toolpkg.ToolDef, 0, len(scanned))
	for _, s := range scanned {
		runtimeDef, err := runtimeToolDef(s.def)
		if err != nil {
			return nil, err
		}
		if err := applySourceProvenance(&runtimeDef, s.path); err != nil {
			return nil, err
		}
		out = append(out, runtimeDef)
	}
	return out, nil
}

// ScanSchemaDir discovers .tool.yaml files and returns schema definitions.
func ScanSchemaDir(dir string) ([]*schema.ToolDef, error) {
	return scanSchemaDir(dir, false)
}

// ScanSchemaDirWithoutTests discovers schema definitions while excluding test
// fixture trees. Production server scans must not bind test-only tool doubles.
func ScanSchemaDirWithoutTests(dir string) ([]*schema.ToolDef, error) {
	return scanSchemaDir(dir, true)
}

func scanSchemaDir(dir string, excludeTests bool) ([]*schema.ToolDef, error) {
	scanned, err := scanSchemaDirWithPaths(dir, excludeTests)
	if err != nil {
		return nil, err
	}
	defs := make([]*schema.ToolDef, 0, len(scanned))
	for _, s := range scanned {
		defs = append(defs, s.def)
	}
	return defs, nil
}

// scannedToolDef pairs a parsed schema tool definition with the absolute path
// of the .tool.yaml file it was parsed from, so the runtime conversion can
// stamp source provenance the schema struct itself does not carry.
type scannedToolDef struct {
	def  *schema.ToolDef
	path string
}

func scanSchemaDirWithPaths(dir string, excludeTests bool) ([]scannedToolDef, error) {
	if dir == "" {
		return nil, nil
	}
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var defs []scannedToolDef
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip vendored/test fixture trees that are known to contain
			// legacy or intentionally malformed tool YAML and would otherwise
			// abort the scan when running from the repo root.
			name := d.Name()
			if path != dir && (name == "testdata" || name == "node_modules" || name == ".git" ||
				(excludeTests && name == "tests")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".tool.yaml") {
			return nil
		}
		def, err := parseToolFile(path)
		if err != nil {
			// Don't let one bad fixture file break the entire scan;
			// surface it on stderr and skip.
			fmt.Fprintf(os.Stderr, "tool scan: skipping %s: %v\n", path, err)
			return nil
		}
		defs = append(defs, scannedToolDef{def: def, path: path})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return defs, nil
}

// ParseToolFile parses a single .tool.yaml file and returns a schema definition.
func ParseToolFile(path string) (*schema.ToolDef, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseToolBytes(data, path, false)
}

func ParseToolBytes(data []byte, path string, metadataOnly bool) (*schema.ToolDef, error) {
	if metadataOnly {
		return parsePresentationMetadata(data)
	}
	var def schema.ToolDef
	if err := yaml.Unmarshal(data, &def); err != nil {
		return nil, err
	}
	if def.Name == "" {
		return nil, fmt.Errorf("tool definition missing name: %s", path)
	}
	if err := validateActionEnums(path, def.Actions); err != nil {
		return nil, err
	}
	if err := validateActionClassifications(path, def.Actions); err != nil {
		return nil, err
	}
	if def.Governance != nil && def.Governance.RequiresApproval != nil && !*def.Governance.RequiresApproval {
		return nil, fmt.Errorf("%s: governance requires-approval must be true when specified", path)
	}
	if err := validateActionOutputContracts(path, def.Actions); err != nil {
		return nil, err
	}
	if err := validateActionResultContracts(path, def.Transport, def.Actions); err != nil {
		return nil, err
	}
	if errs := ValidateTransportConfig(def.Transport); len(errs) > 0 {
		return nil, fmt.Errorf("%s: %w", path, errs[0])
	}
	if err := ValidateVSCodeToolActions(def.Transport, def.Actions); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := ValidateVSCodeInputActions(def.Transport, def.Actions); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := ValidateMCPHTTPActionMappings(def.Transport, def.Actions); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &def, nil
}

// validateActionEnums enforces the enum MVP's S1 (args.<name>) and S2
// (outputs.<name>) well-formedness gate (ENUM-001/003/004/005) on every
// action declared by a .tool.yaml file. ENUM-002 (structural malformation)
// is already enforced by schema.EnumConstraint.UnmarshalYAML during the
// yaml.Unmarshal call above; no JSON Schema governs .tool.yaml (C3, no
// tool.v1.schema.json), so this Go-level pass is the sole enforcement point
// for S1/S2. ENUM-W001 warnings are logged to stderr (there is no
// validation-report channel available at this parse boundary; the
// ValidatedPlan's own enum-metadata warning surface, threaded from the
// planner, is the authoritative channel for the runbook path).
func validateActionEnums(path string, actions map[string]*schema.ToolAction) error {
	for actionName, action := range actions {
		if action == nil {
			continue
		}
		for argName, arg := range action.Args {
			if arg == nil || len(arg.Enum) == 0 {
				continue
			}
			if err := validateOneEnum(path, actionName, "args", argName, arg.Type, arg.Enum); err != nil {
				return err
			}
		}
		for outName, out := range action.Outputs {
			if out == nil || len(out.Enum) == 0 {
				continue
			}
			if err := validateOneEnum(path, actionName, "outputs", outName, out.Type, out.Enum); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateOneEnum(path, actionName, site, name, declType string, enum schema.EnumConstraint) error {
	fatal, warnings := schema.ValidateDeclaration(declType, enum)
	if fatal != nil {
		return fmt.Errorf("%s: action %q %s.%s: %w", path, actionName, site, name, fatal)
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "%s: action %q %s.%s: %v\n", path, actionName, site, name, w)
	}
	return nil
}

func parseToolFile(path string) (*schema.ToolDef, error) {
	return ParseToolFile(path)
}

// RuntimeToolDef converts a schema tool definition to a runtime tool definition.
func RuntimeToolDef(def *schema.ToolDef) (toolpkg.ToolDef, error) {
	if def == nil {
		return toolpkg.ToolDef{}, fmt.Errorf("nil tool definition")
	}
	if err := def.ValidatePresentations(); err != nil {
		return toolpkg.ToolDef{}, err
	}
	transport, err := mapTransport(def.Transport.Type)
	if err != nil {
		return toolpkg.ToolDef{}, err
	}

	// Convert schema actions to runtime actions
	actions := make(map[string]*toolpkg.ToolAction, len(def.Actions))
	for name, schemaAction := range def.Actions {
		if schemaAction == nil {
			continue
		}
		runtimeArgs := make(map[string]*toolpkg.ArgDef, len(schemaAction.Args))
		for argName, schemaArg := range schemaAction.Args {
			if schemaArg == nil {
				continue
			}
			runtimeArgs[argName] = &toolpkg.ArgDef{
				Presentation: schemaArg.Presentation,
				Type:         schemaArg.Type,
				Required:     schemaArg.Required,
				Description:  schemaArg.Description,
				Default:      schemaArg.Default,
				From:         schemaArg.From,
				Optional:     schemaArg.Optional,
				Enum:         schemaArg.Enum,
			}
		}
		action := &toolpkg.ToolAction{
			Description:    schemaAction.Description,
			Argv:           schemaAction.Argv,
			Args:           runtimeArgs,
			Returns:        schemaAction.Returns,
			Execute:        schemaAction.Execute,
			Outputs:        OutputsWithQueryResult(schemaAction.Outputs, schemaAction.Result),
			OutputContract: schemaAction.OutputContract,
			Result:         schemaAction.Result,
			VSCodeInput:    schemaAction.VSCodeInput,
			MCPTool:        schemaAction.MCPTool,
			MCPInput:       schemaAction.MCPInput,
		}
		// Retain the original parsed action so a substituted
		// (execute.kind: runbook) action can be planned via
		// pkg/pkgsubst.Plan without a lossy re-conversion back to schema
		// shape at execution time.
		action = action.WithSchemaAction(schemaAction)
		actions[name] = action
	}

	return toolpkg.ToolDef{
		Name:       def.Name,
		Source:     "tool://" + def.Name,
		Transport:  transport,
		Command:    def.Transport.Command,
		Args:       def.Transport.Args,
		Env:        def.Transport.Env,
		SHA256:     def.Transport.SHA256,
		Inputs:     append([]string(nil), def.Transport.Inputs...),
		URL:        def.Transport.URL,
		Auth:       def.Transport.Auth,
		Actions:    actions,
		Governance: def.Governance,
	}, nil
}

func runtimeToolDef(def *schema.ToolDef) (toolpkg.ToolDef, error) {
	return RuntimeToolDef(def)
}

// OutputsWithQueryResult returns action outputs plus synthesized semantic
// declarations for yawr.query-result/v1. Planner and runtime both use this helper so
// capture validation cannot drift from execution-time outputs.
func OutputsWithQueryResult(outputs map[string]*schema.ArgDef, result *schema.ActionResultContract) map[string]*schema.ArgDef {
	if result == nil {
		return outputs
	}
	merged := make(map[string]*schema.ArgDef, len(outputs)+5)
	for k, v := range outputs {
		merged[k] = v
	}
	defaults := map[string]*schema.ArgDef{
		"success":   {Type: "boolean", Description: "yawr.query-result/v1 success flag"},
		"row_count": {Type: "integer", Description: "yawr.query-result/v1 row count"},
		"columns":   {Type: "array", Description: "yawr.query-result/v1 column names"},
		"rows":      {Type: "array", Description: "yawr.query-result/v1 result rows"},
	}
	if result.Metadata != "" {
		defaults["metadata"] = &schema.ArgDef{Type: "object", Optional: true, Description: "yawr.query-result/v1 metadata"}
	}
	for k, v := range defaults {
		if _, exists := merged[k]; !exists {
			merged[k] = v
		}
	}
	return merged
}

// applySourceProvenance stamps a scanned runtime tool definition with the
// absolute source path of its .tool.yaml and the root/name of the tool
// package that owns it. These fields are what internal/executor.ToolExecutor
// hands to pkg/pkgsubst.Plan (ToolFilePath = SourcePath, PackageRoot,
// packageName) to resolve a substituted (execute.kind: runbook) action's
// execute.path and enforce package containment. A scanned tool that skips
// this arrives at the executor with PackageRoot="" and every legal
// package-internal "../" path fails pkgpath containment as PKG-007.
//
// The owning package root is the nearest ancestor directory that holds a
// yawr-package.yaml manifest -- the same directory pkg/pkgcatalog derives
// from requires: resolution and stamps as PackageRoot. When the tool sits
// under no manifest it is an ad-hoc (non-package) tool whose execute.path is
// contained within its own directory, which is the documented tier-2/4
// fallback on tool.ToolDef.PackageRoot.
func applySourceProvenance(def *toolpkg.ToolDef, toolFilePath string) error {
	abs, err := filepath.Abs(toolFilePath)
	if err != nil {
		abs = toolFilePath
	}
	def.SourcePath = abs
	if root, name, ok, err := owningPackage(abs); err != nil {
		return err
	} else if ok {
		def.PackageRoot = root
		def.PackageName = name
		return nil
	}
	def.PackageRoot = filepath.Dir(abs)
	return nil
}

// owningPackage walks upward from the directory holding toolFilePath and
// returns the first ancestor that contains a yawr-package.yaml manifest,
// together with the package name that manifest declares. found is false when
// no manifest is reachable (an ad-hoc tool outside any package).
func owningPackage(toolFilePath string) (root, name string, found bool, err error) {
	dir := filepath.Dir(toolFilePath)
	for {
		manifestPath := filepath.Join(dir, schema.PackageManifestFilename)
		data, readErr := os.ReadFile(manifestPath)
		if readErr == nil {
			var manifest schema.PackageManifest
			if uerr := yaml.Unmarshal(data, &manifest); uerr == nil {
				name = manifest.Meta.Name
			}
			return dir, name, true, nil
		}
		if !os.IsNotExist(readErr) {
			return "", "", false, readErr
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", false, nil
		}
		dir = parent
	}
}

func mapTransport(t schema.Transport) (toolpkg.TransportType, error) {
	switch t {
	case schema.TransportStdio:
		return toolpkg.TransportStdio, nil
	case schema.TransportJSONRPC:
		return toolpkg.TransportJSONRPC, nil
	case schema.TransportMCP:
		return toolpkg.TransportMCP, nil
	case schema.TransportNative:
		return toolpkg.TransportNative, nil
	case schema.TransportNativeFileOnly:
		return toolpkg.TransportNativeFileOnly, nil
	case schema.TransportMCPHTTP:
		return toolpkg.TransportMCPHTTP, nil
	case schema.TransportVSCodeMCP:
		return toolpkg.TransportVSCodeMCP, nil
	default:
		return "", fmt.Errorf("unsupported transport %q", t)
	}
}

// validClassifications is the closed set of allowed classification values.
var validClassifications = map[string]bool{
	"read-only":   true,
	"mutating":    true,
	"destructive": true,
}

// validateActionClassifications rejects obsolete or unknown policy values.
// Runtime evaluation fails closed if an action omits classification.
func validateActionClassifications(path string, actions map[string]*schema.ToolAction) error {
	for actionName, action := range actions {
		if action == nil || action.Classification == nil {
			continue
		}
		v := *action.Classification
		if !validClassifications[v] {
			return fmt.Errorf("%s: action %q: classification %q is not valid (allowed: read-only, mutating, destructive)", path, actionName, v)
		}
	}
	return nil
}

// validateActionOutputContracts enforces the output_contract policy grammar
// (TOOL-OC1): additional_outputs, when present, must be exactly "ignore".
// An absent block or absent field is the strict default and is always valid.
// An unrecognized value is a hard parse error, never a silent runtime
// fallback, so a typo can never accidentally relax output enforcement.
func validateActionOutputContracts(path string, actions map[string]*schema.ToolAction) error {
	for actionName, action := range actions {
		if action == nil || action.OutputContract == nil {
			continue
		}
		switch action.OutputContract.AdditionalOutputs {
		case "", schema.AdditionalOutputsIgnore:
			// "" = strict default; "ignore" = drop undeclared keys.
		default:
			return fmt.Errorf("%s: action %q: output_contract.additional_outputs %q is not valid (allowed: ignore)", path, actionName, action.OutputContract.AdditionalOutputs)
		}
	}
	return nil
}

func validateActionResultContracts(path string, transport schema.TransportConfig, actions map[string]*schema.ToolAction) error {
	mode := transport.Mode
	if mode == "" {
		mode = string(transport.Type)
	}
	for actionName, action := range actions {
		if action == nil || action.Result == nil {
			continue
		}
		if mode != string(schema.TransportNative) && mode != string(schema.TransportNativeFileOnly) {
			return fmt.Errorf("%s: action %q: result is only supported for native and native-file-only transport actions", path, actionName)
		}
		result := action.Result
		if result.Format != schema.ActionResultFormatQueryResultV1 {
			return fmt.Errorf("%s: action %q: result.format %q is not valid (allowed: %s)", path, actionName, result.Format, schema.ActionResultFormatQueryResultV1)
		}
		if result.Source != schema.ActionResultSourceStdoutJSON {
			return fmt.Errorf("%s: action %q: result.source %q is not valid (allowed: %s)", path, actionName, result.Source, schema.ActionResultSourceStdoutJSON)
		}
		for field, value := range map[string]string{
			"row_count": result.RowCount,
			"columns":   result.Columns,
			"rows":      result.Rows,
		} {
			if !validResultMapping(value) {
				return fmt.Errorf("%s: action %q: result.%s must be a non-empty top-level JSON field name", path, actionName, field)
			}
		}
		if result.Metadata != "" && !validResultMapping(result.Metadata) {
			return fmt.Errorf("%s: action %q: result.metadata must be a top-level JSON field name when present", path, actionName)
		}
		if err := validateQueryResultOutputDeclarations(path, actionName, action.Outputs, result.Metadata != ""); err != nil {
			return err
		}
	}
	return nil
}

func validResultMapping(value string) bool {
	if value == "" {
		return false
	}
	return !strings.ContainsAny(value, ".[]")
}

func validateQueryResultOutputDeclarations(path, actionName string, outputs map[string]*schema.ArgDef, hasMetadata bool) error {
	expected := map[string]struct {
		typ      string
		optional bool
	}{
		"success":   {typ: "boolean"},
		"row_count": {typ: "integer"},
		"columns":   {typ: "array"},
		"rows":      {typ: "array"},
	}
	if hasMetadata {
		expected["metadata"] = struct {
			typ      string
			optional bool
		}{typ: "object", optional: true}
	}
	for name, want := range expected {
		decl, exists := outputs[name]
		if !exists {
			continue
		}
		if decl == nil {
			return fmt.Errorf("%s: action %q: outputs.%s conflicts with result yawr.query-result/v1 semantic output; definition must not be null", path, actionName, name)
		}
		if decl.Type != want.typ || decl.Optional != want.optional || decl.From != "" {
			optionalText := "required"
			if want.optional {
				optionalText = "optional"
			}
			return fmt.Errorf("%s: action %q: outputs.%s conflicts with result yawr.query-result/v1 semantic output; expected type %s, %s, and no from projection", path, actionName, name, want.typ, optionalText)
		}
	}
	if !hasMetadata {
		if decl, exists := outputs["metadata"]; exists {
			if decl == nil {
				return fmt.Errorf("%s: action %q: outputs.metadata conflicts with result yawr.query-result/v1 semantic output; definition must not be null", path, actionName)
			}
			return fmt.Errorf("%s: action %q: outputs.metadata conflicts with result yawr.query-result/v1 because result.metadata is not mapped", path, actionName)
		}
	}
	return nil
}
